package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/migrate"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

// exitError carries a remote command's exit status through to the process.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func passthrough(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitError{ee.ExitCode()}
	}
	return err
}

func (a *app) wpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wp <env> [--] <wp-cli args...>",
		Short: "Run WP-CLI in an environment (e.g. teleport wp production -- plugin list --url=example.com/shop)",
		Long: `Runs WP-CLI in the environment's WordPress root over the configured transport,
with stdin/stdout/stderr attached and the remote exit status returned.
Arguments after -- are passed to WP-CLI unchanged.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			var quoted []string
			for _, arg := range args[1:] {
				quoted = append(quoted, transport.Quote(arg))
			}
			script := env.WPCmd() + " " + strings.Join(quoted, " ")
			tty := isTerminal(os.Stdin) && isTerminal(os.Stdout)
			return passthrough(env.Interactive(cmd.Context(), script, tty).Run())
		},
	}
	// Everything after <env> belongs to WP-CLI, including its --flags.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <env> [command]",
		Short: "Open a shell (or run a command) in an environment's WordPress root",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			script := `exec "${SHELL:-sh}" -l`
			tty := true
			if len(args) == 2 {
				script, tty = args[1], isTerminal(os.Stdin) && isTerminal(os.Stdout)
			}
			return passthrough(env.Interactive(cmd.Context(), script, tty).Run())
		},
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (a *app) exportCmd() *cobra.Command {
	var sel config.Selection
	var out string
	cmd := &cobra.Command{
		Use:   "export <env>",
		Short: "Save a database snapshot to a file (.sql.zst, .sql.gz or .sql; - for stdout)",
		Long: `Dumps the selected tables of <env> into one file. The first line records the
environment's sites, prefixes and URLs, so ` + "`teleport import`" + ` can restore it into any
environment with the same site mapping and URL rewriting as a live migration.
The file is plain SQL after that line, so ` + "`wp db import`" + ` also works on the same site.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if out == "" {
				out = migrate.DefaultSnapshotName(args[0], sel)
			}
			if out == "-" {
				a.u.Quiet = true
			}
			res, err := migrate.Export(cmd.Context(), a.cfg, a.u, args[0], sel, out)
			if err != nil {
				return err
			}
			if out != "-" {
				a.u.Done("exported %d tables to %s (%s) in %s", res.Tables, res.File, ui.Bytes(res.Bytes), ui.Duration(secs(res.Duration)))
				return a.emit(res)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&out, "output", "o", "", "file to write (default <env>-<sites>-<time>.sql.zst)")
	fl.StringSliceVar(&sel.Sites, "sites", nil, "multisite: site slugs to export")
	fl.BoolVar(&sel.Network, "network", false, "multisite: export the whole network, including users")
	fl.StringSliceVar(&sel.Tables, "tables", nil, "only these tables (globs allowed)")
	fl.StringSliceVar(&sel.ExcludeTables, "exclude-tables", nil, "skip these tables (globs allowed)")
	fl.StringSliceVar(&sel.ExcludePostTypes, "exclude-post-types", nil, "skip posts of these types (and their meta)")
	fl.BoolVar(&sel.ExcludeSpam, "exclude-spam", false, "skip spam comments")
	fl.BoolVar(&sel.ExcludeTransients, "exclude-transients", false, "skip transients")
	return cmd
}

func (a *app) importCmd() *cobra.Command {
	var sel config.Selection
	opt := &migrate.Options{}
	cmd := &cobra.Command{
		Use:   "import <env> <file>",
		Short: "Restore a teleport export into an environment (atomic swap, rollback-able)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			res, err := migrate.Import(cmd.Context(), a.cfg, a.u, args[1], args[0], sel, *opt)
			if res != nil {
				a.emit(res)
			}
			if err != nil {
				return err
			}
			if !opt.DryRun {
				a.u.Done("%s -> %s finished in %.1fs", res.From, res.To, res.Duration)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&sel.Sites, "sites", nil, "restore only these sites from the snapshot")
	fl.StringVar(&sel.As, "as", "", "restore the one --sites value into a differently named site")
	fl.BoolVar(&sel.CreateSite, "create-site", false, "create destination sites that do not exist yet")
	fl.StringSliceVar(&sel.Tables, "tables", nil, "restore only these tables (globs allowed)")
	fl.StringSliceVar(&sel.ExcludeTables, "exclude-tables", nil, "skip these tables (globs allowed)")
	fl.BoolVar(&sel.SkipGUIDs, "skip-guids", false, "do not rewrite post GUIDs")
	fl.StringSliceVar(&sel.Preserve, "preserve-options", nil, "keep these destination options (blog_public is always kept)")
	fl.StringArrayVar(&sel.Replace, "replace", nil, "extra find/replace, 'old=>new' (repeatable)")
	fl.StringArrayVar(&sel.Regex, "regex", nil, "regular-expression find/replace, 'pattern=>replacement' (repeatable)")
	fl.BoolVarP(&opt.DryRun, "dry-run", "n", false, "show the plan without changing anything")
	fl.StringVar(&opt.Confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it")
	fl.BoolVar(&opt.NoBackup, "no-backup", false, "drop replaced tables instead of keeping them for rollback")
	fl.BoolVar(&opt.ForceUnlock, "force-unlock", false, "take over a lock left by another migration")
	return cmd
}

func (a *app) pruneCmd() *cobra.Command {
	var keep int
	var dry bool
	var confirm string
	cmd := &cobra.Command{
		Use:   "prune <env>",
		Short: "Drop old backups, orphaned temporary tables and stale credential files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("keep") {
				keep = a.cfg.Backups.Keep
			}
			if confirm == "" {
				confirm = os.Getenv("TELEPORT_CONFIRM")
			}
			var plan struct {
				Backups []string `json:"backups"`
				Orphans []string `json:"orphans"`
				CNF     int      `json:"cnf"`
				Locked  bool     `json:"locked"`
			}
			if err := agent.Call(cmd.Context(), env, "clean", map[string]any{"keep": keep, "dry": true}, &plan); err != nil {
				return err
			}
			a.u.Println("%s: %d backup tables beyond the last %d migrations, %d orphaned temporary tables", env.Name, len(plan.Backups), keep, len(plan.Orphans))
			for _, t := range append(append([]string{}, plan.Backups...), plan.Orphans...) {
				a.u.Debug("  %s", t)
			}
			if plan.Locked {
				a.u.Warn("a migration is running on %s; temporary tables are left alone", env.Name)
			}
			if dry || len(plan.Backups)+len(plan.Orphans) == 0 {
				return a.emit(plan)
			}
			if env.Protected && confirm != env.Name {
				if err := a.u.Confirm(a.u.Red("Dropping tables on "+strings.ToUpper(env.Name)+"."), env.Name); err != nil {
					return err
				}
			}
			if err := agent.Call(cmd.Context(), env, "clean", map[string]any{"keep": keep}, &plan); err != nil {
				return err
			}
			a.u.Done("pruned %s (%d stale credential files removed)", env.Name, plan.CNF)
			return a.emit(plan)
		},
	}
	cmd.Flags().IntVar(&keep, "keep", 0, "migrations whose backups to keep (default backups.keep)")
	cmd.Flags().BoolVarP(&dry, "dry-run", "n", false, "list what would be dropped")
	cmd.Flags().StringVar(&confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it (or TELEPORT_CONFIRM)")
	return cmd
}

func (a *app) verifyCmd() *cobra.Command {
	var sel config.Selection
	cmd := &cobra.Command{
		Use:   "verify <from> <to>",
		Short: "Check that <to> matches what a migration from <from> would produce (URLs, serialized data, authors)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if !sel.HasDB() {
				sel.DB = true
			}
			out, err := migrate.VerifyEnvs(cmd.Context(), a.cfg, a.u, args[0], args[1], sel)
			if out != nil {
				a.emit(out)
			}
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&sel.Sites, "sites", nil, "multisite: site slugs to check")
	fl.BoolVar(&sel.Network, "network", false, "check the whole network")
	fl.StringVar(&sel.As, "as", "", "the one --sites value was copied into this destination slug")
	return cmd
}

func (a *app) replaceCmd() *cobra.Command {
	f := &selFlags{}
	cmd := &cobra.Command{
		Use:   "replace <env>",
		Short: "Find/replace in an environment's database (atomic swap, rollback-able)",
		Long: `Rewrites selected tables in place using the same serialization-safe engine as
a migration. Pass --replace='old=>new' (repeatable) and the usual --sites / --tables
filters. Previous tables are kept for teleport rollback.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			res, err := migrate.Replace(cmd.Context(), a.cfg, a.u, args[0], f.sel, f.opt)
			if res != nil {
				a.emit(res)
			}
			if err != nil {
				return err
			}
			if !f.opt.DryRun {
				a.u.Done("replace on %s finished in %.1fs", args[0], res.Duration)
			}
			return nil
		},
	}
	bindSelection(cmd, f)
	return cmd
}

func (a *app) deleteSiteCmd() *cobra.Command {
	opt := &migrate.Options{}
	cmd := &cobra.Command{
		Use:   "delete-site <env> <slug>",
		Short: "Delete a multisite subsite (never the main site) and its uploads/sites/<id> folder",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			out, err := migrate.DeleteSite(cmd.Context(), a.cfg, a.u, args[0], args[1], *opt)
			if out != nil {
				a.emit(out)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opt.Confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it (or TELEPORT_CONFIRM)")
	return cmd
}

func (a *app) profilesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "List saved profiles from teleport.yml",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if len(a.cfg.Profiles) == 0 {
				a.u.Println("No profiles in %s.", a.cfg.File)
				return a.emit(a.cfg.Profiles)
			}
			var names []string
			for n := range a.cfg.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				p := a.cfg.Profiles[n]
				a.u.Println("  %s  %s -> %s  sites=%s", a.u.Bold(n), p.From, p.To, strings.Join(p.Sites, ","))
			}
			return a.emit(a.cfg.Profiles)
		},
	}
}
