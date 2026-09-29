package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/migrate"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

func (a *app) env(name string) (*transport.Env, error) {
	ec, err := a.cfg.Env(name)
	if err != nil {
		return nil, err
	}
	return transport.New(ec), nil
}

func (a *app) rollbackCmd() *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use:   "rollback <env> [<run-id>]",
		Short: "Swap the tables replaced by the last (or a given) migration back in",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			return a.rollback(cmd.Context(), args[0], optional(args, 1), confirm)
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it (or TELEPORT_CONFIRM)")
	return cmd
}

func optional(args []string, i int) string {
	if len(args) > i {
		return args[i]
	}
	return ""
}

func (a *app) rollback(ctx context.Context, name, run, confirm string) error {
	if confirm == "" {
		confirm = os.Getenv("TELEPORT_CONFIRM")
	}
	env, err := a.env(name)
	if err != nil {
		return err
	}
	if env.Protected && confirm != env.Name {
		if err := a.u.Confirm(a.u.Red("Rolling back tables on "+strings.ToUpper(env.Name)+"."), env.Name); err != nil {
			return err
		}
	}
	var out struct {
		Run agent.Run `json:"run"`
	}
	if err := agent.Call(ctx, env, "rollback", map[string]string{"run": run}, &out); err != nil {
		return err
	}
	if len(out.Run.AlsoRolledBack) > 0 {
		a.u.Done("first rolled back the newer migrations %s", strings.Join(out.Run.AlsoRolledBack, ", "))
	}
	a.u.Done("rolled back %s on %s (%d tables restored, %d removed)", out.Run.ID, env.Name, len(out.Run.Swapped), len(out.Run.CreatedTables))
	a.u.Println("%s", a.u.Dim("Files are not rolled back."))
	return a.emit(out.Run)
}

func (a *app) backupsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "backups", Short: "List or restore the table backups kept by previous migrations"}
	list := &cobra.Command{
		Use:   "list <env>",
		Short: "List migrations that can be rolled back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			var out struct {
				Runs []agent.Run `json:"runs"`
			}
			if err := agent.Call(cmd.Context(), env, "runs", nil, &out); err != nil {
				return err
			}
			if len(out.Runs) == 0 {
				a.u.Println("No migrations recorded on %s.", env.Name)
			}
			for _, r := range out.Runs {
				status := a.u.Green("restorable")
				switch {
				case r.RolledBack:
					status = a.u.Dim("rolled back")
				case !r.Backup:
					status = a.u.Dim("no backup")
				}
				a.u.Println("  %s  %s  %-12s  %d tables  %s", a.u.Bold(r.ID), time.Unix(r.Created, 0).Format("2006-01-02 15:04"), status, len(r.Swapped)+len(r.CreatedTables), r.Description)
			}
			return a.emit(out.Runs)
		},
	}
	var confirm string
	restore := &cobra.Command{
		Use:   "restore <env> <run-id>",
		Short: "Restore the tables a migration replaced",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			return a.rollback(cmd.Context(), args[0], args[1], confirm)
		},
	}
	restore.Flags().StringVar(&confirm, "confirm", "", "skip the typed confirmation for a protected environment by naming it")
	cmd.AddCommand(list, restore, a.pruneCmd())
	return cmd
}

func (a *app) unlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <env>",
		Short: "Release a migration lock left behind by an interrupted run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			if err := agent.Call(cmd.Context(), env, "unlock", nil, nil); err != nil {
				return err
			}
			a.u.Done("unlocked %s", env.Name)
			return nil
		},
	}
}

func (a *app) envsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "envs",
		Short: "List configured environments",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			for _, n := range a.cfg.EnvNames() {
				e := transport.New(a.cfg.Environments[n])
				extra := ""
				if e.Protected {
					extra = a.u.Red(" protected")
				}
				if n == a.cfg.Local {
					extra += a.u.Dim(" (local)")
				}
				a.u.Println("  %s%s  %s", a.u.Bold(e.Label()), extra, a.u.Dim(e.Path))
			}
			return a.emit(a.cfg.Environments)
		},
	}
}

func (a *app) sitesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sites <env>",
		Short: "List the sites of an environment (slugs are what --sites matches)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			env, err := a.env(args[0])
			if err != nil {
				return err
			}
			m, err := agent.GetManifest(cmd.Context(), env)
			if err != nil {
				return err
			}
			for _, s := range m.Sites {
				a.u.Println("  %-18s #%-4d %s", s.Slug(), s.ID, a.u.Dim(s.Home))
			}
			return a.emit(m.Sites)
		},
	}
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [<env>...]",
		Short: "Check connectivity, tools and database access for each environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = a.cfg.EnvNames()
			}
			return a.doctor(cmd.Context(), names)
		},
	}
}

type doctorReport struct {
	Env      string          `json:"env"`
	OK       bool            `json:"ok"`
	Latency  float64         `json:"connect_seconds"`
	Tools    transport.Tools `json:"tools"`
	WP       string          `json:"wp_version,omitempty"`
	DB       string          `json:"db,omitempty"`
	Sites    int             `json:"sites"`
	Problems []string        `json:"problems,omitempty"`
	Notes    []string        `json:"notes,omitempty"`
}

func (a *app) doctor(ctx context.Context, names []string) error {
	u := a.u
	u.Println("%s", u.Bold("This machine"))
	host := doctorReport{Env: "this machine", OK: true}
	r := transport.HostRsync()
	switch {
	case r.Path == "":
		host.Problems = append(host.Problems, "rsync not found ("+transport.InstallHint()+"); file deltas will fall back to full tar copies")
	case !r.Modern():
		host.Problems = append(host.Problems, fmt.Sprintf("%s is %s; %s for zstd compression and live progress", r.Path, rsyncName(r), transport.InstallHint()))
	default:
		host.Notes = append(host.Notes, fmt.Sprintf("rsync %d.%d.%d (%s)", r.Major, r.Minor, r.Patch, r.Path))
	}
	if exec.Command("ssh-add", "-l").Run() != nil {
		host.Notes = append(host.Notes, "ssh-agent has no keys loaded: server-to-server copies (ssh -A) will relay through this machine")
	}
	printReport(u, host)

	reports := make([]doctorReport, len(names))
	sides := make([]*migrate.Side, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			rep := doctorReport{Env: n}
			t0 := time.Now()
			side, err := migrate.Connect(ctx, a.cfg, n)
			rep.Latency = time.Since(t0).Seconds()
			if err != nil {
				rep.Problems = append(rep.Problems, err.Error())
				reports[i] = rep
				return
			}
			sides[i] = side
			m := side.Manifest
			rep.OK = true
			rep.Tools = side.Tools
			rep.WP = m.WPVersion
			rep.DB = fmt.Sprintf("%s %s (%s)", m.DB.Engine, m.DB.Version, m.DB.Name)
			rep.Sites = len(m.Sites)
			if side.Tools.Dump(m.DB.Engine) == "" {
				rep.Problems = append(rep.Problems, "mysqldump / mariadb-dump missing")
			}
			if side.Tools.Client(m.DB.Engine) == "" {
				rep.Problems = append(rep.Problems, "mysql client missing")
			}
			if side.Env.Kind == transport.KindSSH && !side.Tools.Has("zstd") {
				rep.Notes = append(rep.Notes, "zstd missing: database streams will be sent uncompressed")
			}
			if side.Env.Kind == transport.KindSSH && !side.Tools.Has("rsync") {
				rep.Problems = append(rep.Problems, "rsync missing: file deltas unavailable")
			}
			if side.Env.Kind == transport.KindDocker {
				up := side.Env.HostDir(m.ContentDir + "/uploads")
				if up == "" {
					rep.Notes = append(rep.Notes, "uploads are not bind-mounted; files will stream through docker exec")
				} else {
					rep.Notes = append(rep.Notes, "uploads on this machine at "+up)
				}
			}
			if m.Lock != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("locked by %s since %s (teleport unlock %s)", m.Lock.By, time.Unix(m.Lock.Created, 0).Format(time.Kitchen), n))
			}
			if len(rep.Problems) > 0 {
				rep.OK = false
			}
			reports[i] = rep
		}(i, n)
	}
	wg.Wait()

	// Flag two environments that point at the same database.
	for i := range sides {
		for j := i + 1; j < len(sides); j++ {
			if sides[i] == nil || sides[j] == nil {
				continue
			}
			x, y := sides[i].Manifest.DB, sides[j].Manifest.DB
			if x.Server == y.Server && x.Name == y.Name && sides[i].Manifest.BasePrefix == sides[j].Manifest.BasePrefix {
				reports[i].Problems = append(reports[i].Problems, fmt.Sprintf("uses the same database as %s", names[j]))
				reports[i].OK = false
			}
		}
	}

	failed := false
	for _, rep := range reports {
		printReport(u, rep)
		if !rep.OK {
			failed = true
		}
	}
	a.emit(append([]doctorReport{host}, reports...))
	if failed {
		return fmt.Errorf("doctor found problems")
	}
	return nil
}

func rsyncName(r transport.RsyncInfo) string {
	if r.OpenRsync {
		return "Apple's openrsync"
	}
	return fmt.Sprintf("rsync %d.%d.%d", r.Major, r.Minor, r.Patch)
}

func printReport(u *ui.UI, r doctorReport) {
	if r.Env != "this machine" {
		status := u.Green("ok")
		if !r.OK {
			status = u.Red("problem")
		}
		u.Println("%s  %s  %s", u.Bold(r.Env), status, u.Dim(fmt.Sprintf("%.1fs", r.Latency)))
		if r.WP != "" {
			var tools []string
			for k := range r.Tools.Paths {
				tools = append(tools, k)
			}
			sort.Strings(tools)
			u.Println("  WordPress %s, %d sites, %s", r.WP, r.Sites, r.DB)
			u.Println("  %s", u.Dim("tools: "+strings.Join(tools, " ")))
		}
	}
	for _, p := range r.Problems {
		u.Println("  %s %s", u.Red("x"), p)
	}
	for _, n := range r.Notes {
		u.Println("  %s %s", u.Dim("-"), n)
	}
}

const sampleConfig = `# teleport.yml - environments for wp-teleport (https://github.com/cloak-labs/wp-teleport)
# path is the WordPress root as seen by that environment (where wp-config.php or
# wp-cli.yml lives). Database credentials come from WordPress itself.
environments:
  local:
    docker: wordpress        # container name; or remove for a WordPress on this machine
    path: /var/www/html
  staging:
    ssh: staging             # any Host from ~/.ssh/config, or user@host
    path: ~/public_html
  production:
    ssh: production
    path: ~/public_html
    protected: true          # pushes need the environment name typed to confirm

hooks:
  after:
    - wp cache flush

# preserve_options: [blog_public]
# files:
#   exclude: [cache/]
# backups:
#   keep: 3

profiles:
  refresh-local:
    from: production
    to: local
    db: true
    media: true
`

func (a *app) initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a starter teleport.yml in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			a.u = ui.New(a.quiet, a.verbose)
			path := filepath.Join(".", "teleport.yml")
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("teleport.yml already exists")
			}
			if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
				return err
			}
			a.u.Done("wrote teleport.yml; edit it, then run `teleport doctor`")
			return nil
		},
	}
}
