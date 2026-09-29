// Package cli wires the teleport commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/migrate"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

type app struct {
	configPath string
	jsonOut    bool
	quiet      bool
	verbose    bool
	u          *ui.UI
	cfg        *config.Config
}

func (a *app) load() error {
	a.u = ui.New(a.quiet || a.jsonOut, a.verbose)
	path, err := config.Find(a.configPath)
	if err != nil {
		return err
	}
	a.cfg, err = config.Load(path)
	return err
}

func (a *app) emit(v any) error {
	if !a.jsonOut {
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	a := &app{}
	root := &cobra.Command{
		Use:   "teleport",
		Short: "Push and pull WordPress databases and files between environments",
		Long: `teleport moves WordPress databases and files between local, staging and
production over SSH. Servers only need WP-CLI; nothing is installed on them.

Environments live in teleport.yml (searched upward from the current directory).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "path to teleport.yml (default: search upward, or $TELEPORT_CONFIG)")
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "print a JSON result on stdout (implies --quiet)")
	root.PersistentFlags().BoolVarP(&a.quiet, "quiet", "q", false, "only print errors")
	root.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "list every table, rule and hook")

	root.AddCommand(
		a.migrateCmd("pull <env>", "Copy from <env> into the local environment (defaults to --db --media)", func(args []string) (string, string) { return args[0], a.cfg.Local }, 1),
		a.migrateCmd("push <env>", "Copy from the local environment to <env> (defaults to --db --media)", func(args []string) (string, string) { return a.cfg.Local, args[0] }, 1),
		a.migrateCmd("sync <from> <to>", "Copy between any two environments (defaults to --db --media)", func(args []string) (string, string) { return args[0], args[1] }, 2),
		a.diffCmd(),
		a.runCmd(),
		a.rollbackCmd(),
		a.backupsCmd(),
		a.doctorCmd(),
		a.sitesCmd(),
		a.envsCmd(),
		a.unlockCmd(),
		a.initCmd(),
		a.exportCmd(),
		a.importCmd(),
		a.wpCmd(),
		a.shellCmd(),
		a.verifyCmd(),
		a.replaceCmd(),
		a.deleteSiteCmd(),
		a.profilesCmd(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		u := a.u
		if u == nil {
			u = ui.New(false, false)
		}
		fmt.Fprintln(os.Stderr, u.Red("error: ")+err.Error())
		if a.jsonOut {
			a.emit(map[string]any{"ok": false, "error": err.Error()})
		}
		return 1
	}
	return 0
}

type selFlags struct {
	sel config.Selection
	opt migrate.Options
}

func bindSelection(cmd *cobra.Command, f *selFlags) {
	fl := cmd.Flags()
	s := &f.sel
	fl.StringSliceVar(&s.Sites, "sites", nil, "multisite: site slugs to copy (matched by path, not ID); \"main\" is the root site")
	fl.StringVar(&s.As, "as", "", "copy the one --sites value into a differently named destination site")
	fl.BoolVar(&s.Network, "network", false, "multisite: copy the whole network, including users")
	fl.BoolVar(&s.CreateSite, "create-site", false, "create destination sites that do not exist yet")

	fl.BoolVar(&s.DB, "db", false, "copy the database tables of the selected sites")
	fl.StringSliceVar(&s.Tables, "tables", nil, "only these tables (unprefixed or full names, globs allowed: posts,postmeta,gf_*); implies --db")
	fl.StringSliceVar(&s.ExcludeTables, "exclude-tables", nil, "skip these tables (unprefixed or full names, globs allowed)")
	fl.StringSliceVar(&s.ExcludePostTypes, "exclude-post-types", nil, "skip posts of these types (and their meta), e.g. revision")
	fl.BoolVar(&s.ExcludeRevisions, "exclude-revisions", false, "skip post revisions (and their meta)")
	fl.BoolVar(&s.ExcludeSpam, "exclude-spam", false, "skip spam comments")
	fl.BoolVar(&s.ExcludeTransients, "exclude-transients", false, "skip transients")
	fl.BoolVar(&s.SkipGUIDs, "skip-guids", false, "do not rewrite post GUIDs")
	fl.BoolVar(&s.Users, "users", false, "also copy the network-wide users tables (overwrites destination users)")
	fl.StringSliceVar(&s.Preserve, "preserve-options", nil, "keep these destination options (blog_public is always kept)")
	fl.BoolVar(&s.PreservePlugins, "preserve-active-plugins", false, "keep the destination's active plugins and theme")
	fl.StringArrayVar(&s.Replace, "replace", nil, "extra find/replace, 'old=>new' (repeatable)")
	fl.StringArrayVar(&s.Regex, "regex", nil, "regular-expression find/replace, 'pattern=>replacement' (repeatable)")

	fl.BoolVar(&s.Media, "media", false, "copy uploads of the selected sites")
	fl.StringVar(&s.MediaSince, "media-since", "", "only files modified since this date (YYYY, YYYY-MM or YYYY-MM-DD)")
	fl.BoolVar(&s.Delete, "delete", false, "delete destination files missing from the source")
	fl.StringSliceVar(&s.Themes, "themes", nil, "copy themes (all, or a comma list)")
	fl.Lookup("themes").NoOptDefVal = "*"
	fl.StringSliceVar(&s.Plugins, "plugins", nil, "copy plugins (all, or a comma list)")
	fl.Lookup("plugins").NoOptDefVal = "*"
	fl.BoolVar(&s.MuPlugins, "mu-plugins", false, "copy mu-plugins")
	fl.StringSliceVar(&s.Files, "files", nil, "copy other wp-content paths, e.g. languages")
	fl.StringArrayVar(&s.Exclude, "exclude", nil, "rsync-style file pattern to skip, e.g. '*.mp4' or 'cache/' (repeatable)")

	bindOptions(cmd, &f.opt)
}

func bindOptions(cmd *cobra.Command, o *migrate.Options) {
	fl := cmd.Flags()
	fl.BoolVarP(&o.DryRun, "dry-run", "n", false, "show the plan and transfer sizes without changing anything")
	fl.StringVar(&o.Confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it (or TELEPORT_CONFIRM)")
	fl.BoolVar(&o.Full, "full", false, "copy every table even if checksums say it is unchanged")
	fl.BoolVar(&o.NoBackup, "no-backup", false, "drop replaced tables instead of keeping them for rollback")
	fl.IntVar(&o.Keep, "keep", 0, "migrations whose table backups to keep (0 = config default, -1 = none)")
	fl.BoolVar(&o.Maintenance, "maintenance", false, "enable maintenance mode on the destination while migrating")
	fl.BoolVar(&o.ForceUnlock, "force-unlock", false, "take over a lock left by another migration")
	fl.IntVarP(&o.Parallel, "parallel", "j", 0, "parallel database streams and rsync shards (default from config, 4)")
	fl.BoolVar(&o.Verify, "verify", false, "after swapping, check leftover source URLs, serialized data and home/siteurl")
}

func (a *app) migrateCmd(use, short string, route func([]string) (string, string), nargs int) *cobra.Command {
	f := &selFlags{}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(nargs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			from, to := route(args)
			return a.migrate(cmd.Context(), from, to, f.sel, f.opt)
		},
	}
	bindSelection(cmd, f)
	return cmd
}

func (a *app) migrate(ctx context.Context, from, to string, sel config.Selection, opt migrate.Options) error {
	res, err := migrate.Run(ctx, a.cfg, a.u, from, to, sel, opt)
	if res != nil {
		a.emit(res)
	}
	if err != nil {
		return err
	}
	if !opt.DryRun {
		a.u.Done("%s -> %s finished in %.1fs", from, to, res.Duration)
	}
	return nil
}

func (a *app) diffCmd() *cobra.Command {
	f := &selFlags{}
	cmd := &cobra.Command{
		Use:   "diff <from> [<to>]",
		Short: "Preview a migration: tables that changed, rows, and file bytes to move",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			to := a.cfg.Local
			if len(args) == 2 {
				to = args[1]
			}
			f.opt.DryRun = true
			if !f.sel.HasDB() && !f.sel.HasFiles() {
				f.sel.DB, f.sel.Media = true, true
			}
			return a.migrate(cmd.Context(), args[0], to, f.sel, f.opt)
		},
	}
	bindSelection(cmd, f)
	return cmd
}

func (a *app) runCmd() *cobra.Command {
	f := &selFlags{}
	cmd := &cobra.Command{
		Use:   "run <profile>",
		Short: "Run a saved profile from teleport.yml (flags override the profile)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			p, ok := a.cfg.Profiles[args[0]]
			if !ok {
				var names []string
				for n := range a.cfg.Profiles {
					names = append(names, n)
				}
				return fmt.Errorf("no profile %q (have: %v)", args[0], names)
			}
			sel := overlaySelection(p, f.sel, cmd.Flags())
			return a.migrate(cmd.Context(), p.From, p.To, sel, f.opt)
		},
	}
	bindSelection(cmd, f)
	return cmd
}

func overlaySelection(base, over config.Selection, flags interface{ Changed(string) bool }) config.Selection {
	s := base
	set := func(name string, apply func()) {
		if flags.Changed(name) {
			apply()
		}
	}
	set("sites", func() { s.Sites = over.Sites })
	set("as", func() { s.As = over.As })
	set("network", func() { s.Network = over.Network })
	set("create-site", func() { s.CreateSite = over.CreateSite })
	set("db", func() { s.DB = over.DB })
	set("tables", func() { s.Tables = over.Tables })
	set("exclude-tables", func() { s.ExcludeTables = over.ExcludeTables })
	set("exclude-post-types", func() { s.ExcludePostTypes = over.ExcludePostTypes })
	set("exclude-revisions", func() { s.ExcludeRevisions = over.ExcludeRevisions })
	set("exclude-spam", func() { s.ExcludeSpam = over.ExcludeSpam })
	set("exclude-transients", func() { s.ExcludeTransients = over.ExcludeTransients })
	set("skip-guids", func() { s.SkipGUIDs = over.SkipGUIDs })
	set("users", func() { s.Users = over.Users })
	set("preserve-options", func() { s.Preserve = over.Preserve })
	set("preserve-active-plugins", func() { s.PreservePlugins = over.PreservePlugins })
	set("replace", func() { s.Replace = over.Replace })
	set("regex", func() { s.Regex = over.Regex })
	set("media", func() { s.Media = over.Media })
	set("media-since", func() { s.MediaSince = over.MediaSince })
	set("delete", func() { s.Delete = over.Delete })
	set("themes", func() { s.Themes = over.Themes })
	set("plugins", func() { s.Plugins = over.Plugins })
	set("mu-plugins", func() { s.MuPlugins = over.MuPlugins })
	set("files", func() { s.Files = over.Files })
	set("exclude", func() { s.Exclude = over.Exclude })
	return s
}
