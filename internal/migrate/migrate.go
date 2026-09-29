// Package migrate orchestrates one push/pull: probe both ends, plan, confirm,
// stream tables into temporary tables, swap atomically, copy files, run hooks.
package migrate

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/db"
	"github.com/cloak-labs/wp-teleport/internal/files"
	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/sqlstream"
	"github.com/cloak-labs/wp-teleport/internal/state"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

type Options struct {
	DryRun      bool
	Confirm     string
	Full        bool
	NoBackup    bool
	Keep        int
	Maintenance bool
	ForceUnlock bool
	Parallel    int
	Verify      bool
}

type Result struct {
	OK       bool           `json:"ok"`
	Run      string         `json:"run"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	DryRun   bool           `json:"dry_run,omitempty"`
	Plan     *plan.Plan     `json:"plan"`
	Created  []string       `json:"created_sites,omitempty"`
	DB       *db.Result     `json:"db,omitempty"`
	Skipped  int            `json:"unchanged_tables"`
	Files    []files.Result `json:"files,omitempty"`
	Backup   bool           `json:"backup"`
	Verify   *VerifyResult  `json:"verify,omitempty"`
	Duration float64        `json:"duration_seconds"`
}

// Side is a connected environment.
type Side struct {
	Env      *transport.Env
	Tools    transport.Tools
	Manifest *agent.Manifest
}

// Connect probes tools and fetches the manifest.
func Connect(ctx context.Context, cfg *config.Config, name string) (*Side, error) {
	ec, err := cfg.Env(name)
	if err != nil {
		return nil, err
	}
	env := transport.New(ec)
	tools, err := env.Tools(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", env.Label(), err)
	}
	m, err := agent.GetManifest(ctx, env)
	if err != nil {
		return nil, err
	}
	return &Side{Env: env, Tools: tools, Manifest: m}, nil
}

func connectBoth(ctx context.Context, cfg *config.Config, from, to string) (*Side, *Side, error) {
	var src, dst *Side
	var srcErr, dstErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); src, srcErr = Connect(ctx, cfg, from) }()
	go func() { defer wg.Done(); dst, dstErr = Connect(ctx, cfg, to) }()
	wg.Wait()
	if srcErr != nil {
		return nil, nil, srcErr
	}
	if dstErr != nil {
		return nil, nil, dstErr
	}
	return src, dst, nil
}

func newRunID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func who() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name + "@" + host
}

// Run performs (or, with DryRun, previews) a migration.
func Run(ctx context.Context, cfg *config.Config, u *ui.UI, from, to string, sel config.Selection, opt Options) (*Result, error) {
	start := time.Now()
	if from == to {
		return nil, fmt.Errorf("source and destination are both %q", from)
	}
	sel.Normalize()
	if !sel.HasDB() && !sel.HasFiles() {
		sel.DB, sel.Media = true, true
	}
	applyConfirm(&opt)
	if opt.Parallel <= 0 {
		opt.Parallel = cfg.Parallel
	}
	if opt.Keep != 0 {
		cfg.Backups.Keep = opt.Keep
	}
	u.Step("Connecting to %s and %s", from, to)
	src, dst, err := connectBoth(ctx, cfg, from, to)
	if err != nil {
		return nil, err
	}
	res := &Result{Run: newRunID(), From: from, To: to, DryRun: opt.DryRun}
	p, err := plan.Build(src.Manifest, dst.Manifest, sel, cfg, res.Run)
	if err != nil {
		return nil, err
	}
	res.Plan = p

	st := state.Load(cfg.Dir)
	key := state.Key(from, to)
	srcSums, err := checksumSource(ctx, u, src, dst, p, st, key, opt)
	if err != nil {
		return nil, err
	}
	for _, t := range p.Tables {
		if t.Unchanged {
			res.Skipped++
		}
	}
	printPlan(u, src, dst, p, sel, cfg, opt)

	if opt.DryRun {
		for _, job := range p.Files {
			r, err := files.Transfer(ctx, fileSide(src), fileSide(dst), job, files.Options{Parallel: 1, DryRun: true, Rsync: transport.HostRsync(), UI: u})
			if err != nil {
				u.Warn("%s: %v", job.Label, err)
			}
			res.Files = append(res.Files, r)
		}
		printDryRunFiles(u, res.Files)
		res.Duration = time.Since(start).Seconds()
		res.OK = true
		return res, nil
	}

	if dst.Env.Protected && opt.Confirm != dst.Env.Name {
		if err := u.Confirm(u.Red(fmt.Sprintf("This overwrites data on %s.", strings.ToUpper(dst.Env.Name))), dst.Env.Name); err != nil {
			return nil, err
		}
	}

	if creates := p.NeedsCreate(); len(creates) > 0 {
		for _, c := range creates {
			var out struct {
				ID int `json:"id"`
			}
			if err := agent.Call(ctx, dst.Env, "create-site", map[string]string{"slug": c.Slug, "title": c.Src.Title}, &out); err != nil {
				return nil, err
			}
			u.Done("created site %s on %s (#%d)", c.Slug, to, out.ID)
			res.Created = append(res.Created, c.Slug)
		}
		if dst.Manifest, err = agent.GetManifest(ctx, dst.Env); err != nil {
			return nil, err
		}
		if p, err = plan.Build(src.Manifest, dst.Manifest, sel, cfg, res.Run); err != nil {
			return nil, err
		}
		res.Plan = p
	}

	if err := execute(ctx, cfg, u, source{side: src}, dst, p, sel, opt, res, st, key, srcSums); err != nil {
		return res, err
	}
	if opt.Verify {
		v, err := Verify(ctx, u, dst, p)
		res.Verify = v
		if err != nil {
			res.Duration = time.Since(start).Seconds()
			return res, err
		}
	}
	res.Duration = time.Since(start).Seconds()
	res.OK = true
	return res, nil
}

func applyConfirm(opt *Options) {
	if opt.Confirm == "" {
		opt.Confirm = os.Getenv("TELEPORT_CONFIRM")
	}
}

func fileSide(s *Side) files.Side { return files.Side{Env: s.Env, Tools: s.Tools} }

// checksumSource returns source checksums (recorded after the run) and marks
// tables unchanged since the last identical migration.
func checksumSource(ctx context.Context, u *ui.UI, src, dst *Side, p *plan.Plan, st *state.State, key string, opt Options) (map[string]string, error) {
	if len(p.Tables) == 0 {
		return nil, nil
	}
	var srcTables, dstTables []string
	for _, t := range p.Tables {
		srcTables = append(srcTables, t.Src)
		dstTables = append(dstTables, t.Live)
	}
	usePrev := !opt.Full && len(st.Pairs[key]) > 0
	var srcSums, dstSums map[string]string
	var srcErr, dstErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var out struct {
			Checksums map[string]string `json:"checksums"`
		}
		srcErr = agent.Call(ctx, src.Env, "checksum", map[string]any{"tables": srcTables}, &out)
		srcSums = out.Checksums
	}()
	if usePrev {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out struct {
				Checksums map[string]string `json:"checksums"`
			}
			dstErr = agent.Call(ctx, dst.Env, "checksum", map[string]any{"tables": dstTables}, &out)
			dstSums = out.Checksums
		}()
	}
	wg.Wait()
	if srcErr != nil {
		return nil, srcErr
	}
	if usePrev && dstErr == nil {
		if n := st.MarkUnchanged(key, p, srcSums, dstSums); n > 0 {
			u.Debug("%d tables unchanged since the last migration", n)
		}
	}
	return srcSums, nil
}

// source is where tables come from: a live environment, or a snapshot file.
type source struct {
	side *Side
	snap io.Reader
	name string
}

func (s source) label() string {
	if s.side != nil {
		return s.side.Env.Name
	}
	return s.name
}

func execute(ctx context.Context, cfg *config.Config, u *ui.UI, from source, dst *Side, p *plan.Plan, sel config.Selection, opt Options, res *Result, st *state.State, key string, srcSums map[string]string) (err error) {
	src := from.side
	var jobs []plan.TableJob
	for _, t := range p.Tables {
		if !t.Unchanged {
			jobs = append(jobs, t)
		}
	}
	hasDB := len(jobs) > 0
	run := p.Run

	begin := map[string]any{"lock": map[string]any{"by": who(), "run": run, "force": opt.ForceUnlock}}
	if hasDB {
		begin["cnf"] = run
	}
	var dstBegin agent.BeginResult
	if err := agent.Call(ctx, dst.Env, "begin", begin, &dstBegin); err != nil {
		if !strings.Contains(err.Error(), "holds the lock") {
			abort(dst, run, nil, true)
		}
		return err
	}
	var srcBegin agent.BeginResult
	if hasDB && src != nil {
		if err := agent.Call(ctx, src.Env, "begin", map[string]any{"cnf": run}, &srcBegin); err != nil {
			abort(dst, run, nil, true)
			return err
		}
	}
	swapped := false
	defer func() {
		if err != nil && !swapped {
			var tmps []string
			for _, t := range jobs {
				tmps = append(tmps, t.Tmp)
			}
			abort(dst, run, tmps, true)
			if hasDB && src != nil {
				abort(src, run, nil, false)
			}
			u.Warn("migration failed; the destination was not changed")
		} else if err != nil {
			agent.Call(context.WithoutCancel(ctx), dst.Env, "unlock", nil, nil)
		}
	}()

	if err := runHooks(ctx, u, dst, p, append(append([]string{}, cfg.Hooks.Before...), dst.Env.Hooks.Before...), res); err != nil {
		return err
	}
	if opt.Maintenance {
		if _, err := dst.Env.Run(ctx, dst.Env.WPCmd()+" maintenance-mode activate", nil); err != nil {
			u.Warn("could not enable maintenance mode: %v", err)
		} else {
			defer dst.Env.Run(context.WithoutCancel(ctx), dst.Env.WPCmd()+" maintenance-mode deactivate", nil)
		}
	}

	keep := cfg.Backups.Keep
	backup := !opt.NoBackup && keep > 0
	res.Backup = backup

	if hasDB {
		var bytes int64
		for _, j := range jobs {
			bytes += j.Bytes
		}
		u.Step("Database: %d tables (%s) from %s to %s", len(jobs), ui.Bytes(bytes), from.label(), dst.Env.Name)
		meter := u.Meter("db", 0)
		dstEP := db.Endpoint{Env: dst.Env, Tools: dst.Tools, Engine: dst.Manifest.DB.Engine, DBName: dst.Manifest.DB.Name, Cnf: dstBegin.Cnf}
		opts := sqlstream.Options{
			Replacer:     sqlstream.NewReplacer(p.Pairs, p.Regexes),
			Collations:   p.Collations,
			SkipColumns:  p.SkipColumns,
			ExactColumns: p.ExactColumns,
		}
		t0 := time.Now()
		var dres *db.Result
		if src != nil {
			srcEP := db.Endpoint{Env: src.Env, Tools: src.Tools, Engine: src.Manifest.DB.Engine, DBName: src.Manifest.DB.Name, Cnf: srcBegin.Cnf}
			dres, err = db.Transfer(ctx, srcEP, dstEP, jobs, opts, opt.Parallel, meter)
		} else {
			dres, err = db.Import(ctx, from.snap, dstEP, jobs, opts, meter)
		}
		meter.Stop()
		res.DB = dres
		if err != nil {
			return err
		}
		u.Done("imported %d tables in %s (%s of SQL, %d values rewritten, %d streams)", dres.Tables, ui.Duration(time.Since(t0)), ui.Bytes(dres.Stats.InBytes), dres.Stats.ChangedValues, dres.Streams)

		var swap []map[string]string
		var live []string
		transferred := map[string]bool{}
		for _, j := range jobs {
			swap = append(swap, map[string]string{"tmp": j.Tmp, "live": j.Live})
			live = append(live, j.Live)
			transferred[j.Tmp] = true
		}
		var preservePairs []plan.PreservePair
		for _, pp := range p.PreservePairs {
			if transferred[pp.Tmp] {
				preservePairs = append(preservePairs, pp)
			}
		}
		fin := map[string]any{
			"run":            run,
			"swap":           swap,
			"keep_backup":    backup,
			"description":    fmt.Sprintf("%s -> %s %s", from.label(), dst.Env.Name, describe(sel)),
			"preserve":       p.Preserve,
			"preserve_pairs": preservePairs,
			"checksum":       live,
			"cleanup":        run,
		}
		if backup {
			fin["prune"] = keep
		}
		var out agent.FinalizeResult
		if err := agent.Call(ctx, dst.Env, "finalize", fin, &out); err != nil {
			return err
		}
		swapped = true
		if src != nil {
			agent.Call(context.WithoutCancel(ctx), src.Env, "abort", map[string]any{"cleanup": run}, nil)
		}
		if backup {
			u.Done("swapped in atomically; previous tables kept for `teleport rollback %s`", dst.Env.Name)
		} else {
			u.Done("swapped in atomically (no backup kept)")
		}
		if st != nil {
			st.Record(key, p, srcSums, out.Checksums)
			if err := st.Save(); err != nil {
				u.Warn("could not save state: %v", err)
			}
		}
	} else if len(p.Tables) > 0 {
		u.Done("database: all %d tables unchanged since the last migration (use --full to force)", len(p.Tables))
	}

	rsync := transport.HostRsync()
	for _, job := range p.Files {
		u.Step("Files: %s", job.Label)
		t0 := time.Now()
		r, err := files.Transfer(ctx, fileSide(src), fileSide(dst), job, files.Options{Parallel: opt.Parallel, Rsync: rsync, UI: u})
		res.Files = append(res.Files, r)
		if err != nil {
			return err
		}
		msg := fmt.Sprintf("%s via %s in %s", job.Label, r.Strategy, ui.Duration(time.Since(t0)))
		if r.Files > 0 || r.Bytes > 0 {
			msg += fmt.Sprintf(" (%d files, %s)", r.Files, ui.Bytes(r.Bytes))
		}
		if r.Note != "" {
			msg += " - " + r.Note
		}
		u.Done("%s", msg)
	}

	if err := runHooks(ctx, u, dst, p, append(append([]string{}, cfg.Hooks.After...), dst.Env.Hooks.After...), res); err != nil {
		u.Warn("%v", err)
	}
	if err := agent.Call(ctx, dst.Env, "unlock", nil, nil); err != nil {
		u.Warn("could not release lock: %v", err)
	}
	return nil
}

func abort(side *Side, run string, drop []string, unlock bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	agent.Call(ctx, side.Env, "abort", map[string]any{"drop": drop, "cleanup": run, "unlock": unlock}, nil)
}

func describe(sel config.Selection) string {
	var parts []string
	if sel.Network {
		parts = append(parts, "network")
	}
	if len(sel.Sites) > 0 {
		parts = append(parts, "sites="+strings.Join(sel.Sites, ","))
	}
	if len(sel.Tables) > 0 {
		parts = append(parts, "tables="+strings.Join(sel.Tables, ","))
	}
	if sel.Media {
		parts = append(parts, "media")
	}
	return strings.Join(parts, " ")
}

// runHooks runs shell hooks on the destination. A leading `wp ` uses the
// environment's WP-CLI command; `{url}` runs the hook once per migrated site.
func runHooks(ctx context.Context, u *ui.UI, dst *Side, p *plan.Plan, hooks []string, res *Result) error {
	var urls, slugs []string
	for _, s := range p.Sites {
		if s.Dst != nil {
			urls = append(urls, s.Dst.Home)
			slugs = append(slugs, s.Slug)
		}
	}
	env := fmt.Sprintf("TELEPORT_FROM=%s TELEPORT_TO=%s TELEPORT_RUN=%s TELEPORT_SITES=%s ",
		transport.Quote(res.From), transport.Quote(res.To), transport.Quote(res.Run), transport.Quote(strings.Join(slugs, ",")))
	for _, h := range hooks {
		cmd := h
		if rest, ok := strings.CutPrefix(cmd, "wp "); ok {
			cmd = dst.Env.WPCmd() + " " + rest
		}
		targets := []string{cmd}
		if strings.Contains(cmd, "{url}") {
			targets = nil
			for _, url := range urls {
				targets = append(targets, strings.ReplaceAll(cmd, "{url}", transport.Quote(url)))
			}
		}
		for _, t := range targets {
			u.Debug("hook: %s", t)
			if _, err := dst.Env.Run(ctx, env+t, nil); err != nil {
				return fmt.Errorf("hook %q failed: %w", h, err)
			}
		}
		u.Done("hook: %s", h)
	}
	return nil
}

func printPlan(u *ui.UI, src, dst *Side, p *plan.Plan, sel config.Selection, cfg *config.Config, opt Options) {
	title := fmt.Sprintf("%s -> %s", src.Env.Label(), dst.Env.Label())
	if dst.Env.Protected {
		title += " " + u.Red("[protected]")
	}
	u.Println("")
	u.Println("%s  %s", u.Bold("Plan"), title)
	if p.Network {
		u.Println("  %-9s entire network (%d sites)", "scope", len(p.Sites))
	} else {
		for _, s := range p.Sites {
			switch {
			case s.Create:
				u.Println("  %-9s %s (#%d) -> %s", "site", s.Src.Slug(), s.Src.ID, u.Yellow("new site "+s.Slug))
			case s.Dst != nil:
				u.Println("  %-9s %s (#%d) -> %s (#%d)", "site", s.Src.Slug(), s.Src.ID, s.Slug, s.Dst.ID)
			}
		}
	}
	if len(p.Tables) > 0 {
		var bytes int64
		unchanged := 0
		for _, t := range p.Tables {
			if t.Unchanged {
				unchanged++
			} else {
				bytes += t.Bytes
			}
		}
		line := fmt.Sprintf("%d tables, ~%s", len(p.Tables)-unchanged, ui.Bytes(bytes))
		if unchanged > 0 {
			line += u.Dim(fmt.Sprintf(" (%d unchanged, skipped)", unchanged))
		}
		u.Println("  %-9s %s", "database", line)
		if u.Verbose {
			for _, t := range p.Tables {
				mark := ""
				if t.Unchanged {
					mark = " (unchanged)"
				}
				filter := ""
				if t.Where != "" {
					filter = u.Dim(" where " + t.Where)
				}
				u.Println("              %s -> %s  %s%s%s", t.Src, t.Live, ui.Bytes(t.Bytes), mark, filter)
			}
		}
	} else if sel.HasDB() && len(p.NeedsCreate()) == 0 {
		u.Println("  %-9s no matching tables", "database")
	}
	if len(p.Tables) > 0 && len(p.Pairs) > 0 {
		shown := 0
		for _, pr := range p.Pairs {
			if strings.Contains(pr.From, `\/`) {
				continue
			}
			if shown < 4 || u.Verbose {
				u.Println("  %-9s %s -> %s", "replace", pr.From, pr.To)
			}
			shown++
		}
		if shown > 4 && !u.Verbose {
			u.Println("  %-9s %s", "", u.Dim(fmt.Sprintf("+%d more (JSON-escaped variants included; -v to list)", shown-4)))
		}
	}
	if len(p.Collations) > 0 && len(p.Tables) > 0 {
		u.Println("  %-9s %s -> %s collations mapped", "engine", src.Manifest.DB.Engine, dst.Manifest.DB.Engine)
	}
	if len(p.PreservePairs) > 0 {
		u.Println("  %-9s %s", "preserve", strings.Join(p.Preserve, ", "))
	}
	for _, f := range p.Files {
		extra := ""
		if f.Delete {
			extra += " " + u.Yellow("--delete")
		}
		if f.Since != "" {
			extra += " since " + f.Since
		}
		u.Println("  %-9s %s  %s -> %s%s", "files", f.Label, u.Dim(f.Src), u.Dim(f.Dst), extra)
	}
	if len(p.Tables) > 0 {
		if !opt.NoBackup && cfg.Backups.Keep > 0 {
			u.Println("  %-9s previous tables kept for rollback (last %d migrations)", "backup", cfg.Backups.Keep)
		} else {
			u.Println("  %-9s %s", "backup", u.Yellow("none"))
		}
	}
	for _, w := range p.Warnings {
		u.Warn("%s", w)
	}
	u.Println("")
}

func printDryRunFiles(u *ui.UI, results []files.Result) {
	for _, r := range results {
		if r.Strategy == "skip" {
			u.Println("  %-9s %s: %s", "files", r.Label, r.Note)
			continue
		}
		u.Println("  %-9s %s: %d files, %s to transfer via %s", "files", r.Label, r.Files, ui.Bytes(r.Bytes), r.Strategy)
	}
	u.Println("%s", u.Dim("Dry run: nothing was changed."))
}
