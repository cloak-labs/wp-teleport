package migrate

import (
	"context"
	"fmt"
	"strings"

	"time"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/state"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

type VerifySite struct {
	Slug             string   `json:"slug"`
	Prefix           string   `json:"prefix"`
	OK               bool     `json:"ok"`
	Home             string   `json:"home"`
	SiteURL          string   `json:"siteurl"`
	Leftover         int      `json:"leftover"`
	BrokenSerialized int      `json:"broken_serialized"`
	Serialized       int      `json:"serialized"`
	MissingAuthors   int      `json:"missing_authors"`
	Problems         []string `json:"problems,omitempty"`
}

type VerifyResult struct {
	OK    bool         `json:"ok"`
	Sites []VerifySite `json:"sites"`
}

// Verify checks the destination sites of p: expected home/siteurl, leftover
// source URLs, broken serialized PHP, and posts whose authors are missing.
func Verify(ctx context.Context, u *ui.UI, dst *Side, p *plan.Plan) (*VerifyResult, error) {
	needles := verifyNeedles(p)
	var sites []map[string]string
	for _, s := range p.Sites {
		if s.Dst == nil {
			continue
		}
		sites = append(sites, map[string]string{
			"slug": s.Slug, "prefix": s.Dst.Prefix, "home": s.Dst.Home, "siteurl": s.Dst.SiteURL,
		})
	}
	if len(sites) == 0 {
		return &VerifyResult{OK: true}, nil
	}
	u.Step("Verifying %d site(s) on %s", len(sites), dst.Env.Name)
	var out VerifyResult
	if err := agent.Call(ctx, dst.Env, "verify", map[string]any{"needles": needles, "sites": sites}, &out); err != nil {
		return nil, err
	}
	for _, s := range out.Sites {
		if s.OK {
			u.Done("%s: home %s, %d serialized values ok", s.Slug, s.Home, s.Serialized)
			continue
		}
		for _, p := range s.Problems {
			u.Warn("%s: %s", s.Slug, p)
		}
	}
	if !out.OK {
		return &out, fmt.Errorf("verification failed on %s", dst.Env.Name)
	}
	return &out, nil
}

func verifyNeedles(p *plan.Plan) []string {
	seen := map[string]bool{}
	var out []string
	for _, pr := range p.Pairs {
		if strings.Contains(pr.From, `\/`) {
			continue
		}
		if !strings.Contains(pr.From, "://") && !strings.Contains(pr.From, ".") {
			continue
		}
		if len(pr.From) < 8 || seen[pr.From] {
			continue
		}
		seen[pr.From] = true
		out = append(out, pr.From)
	}
	return out
}

// VerifyEnvs connects from and to, plans the same mapping a migration would,
// and checks the destination.
func VerifyEnvs(ctx context.Context, cfg *config.Config, u *ui.UI, from, to string, sel config.Selection) (*VerifyResult, error) {
	sel.Normalize()
	if !sel.HasDB() {
		sel.DB = true
	}
	src, dst, err := connectBoth(ctx, cfg, from, to)
	if err != nil {
		return nil, err
	}
	p, err := plan.Build(src.Manifest, dst.Manifest, sel, cfg, "verify")
	if err != nil {
		return nil, err
	}
	return Verify(ctx, u, dst, p)
}

type DeletedSite struct {
	ID             int      `json:"id"`
	Slug           string   `json:"slug"`
	Prefix         string   `json:"prefix"`
	UploadsDir     string   `json:"uploads_dir"`
	DroppedBackups []string `json:"dropped_backups,omitempty"`
	UploadsRemoved bool     `json:"uploads_removed,omitempty"`
}

// DeleteSite drops a multisite subsite (never the main site) and its leftover
// teleport backup tables. Uploads under uploads/sites/<id> are removed too.
func DeleteSite(ctx context.Context, cfg *config.Config, u *ui.UI, envName, slug string, opt Options) (*DeletedSite, error) {
	applyConfirm(&opt)
	env, err := cfg.Env(envName)
	if err != nil {
		return nil, err
	}
	side, err := Connect(ctx, cfg, envName)
	if err != nil {
		return nil, err
	}
	if side.Env.Protected && opt.Confirm != side.Env.Name {
		if err := u.Confirm(u.Red(fmt.Sprintf("This deletes site %s on %s.", slug, strings.ToUpper(side.Env.Name))), side.Env.Name); err != nil {
			return nil, err
		}
	}
	var out DeletedSite
	if err := agent.Call(ctx, side.Env, "delete-site", map[string]string{"slug": slug}, &out); err != nil {
		return nil, err
	}
	if uploadsSafe(out.UploadsDir) {
		if _, err := side.Env.Run(ctx, "rm -rf "+transport.Quote(out.UploadsDir), nil); err != nil {
			u.Warn("deleted the site but could not remove %s: %v", out.UploadsDir, err)
		} else {
			out.UploadsRemoved = true
		}
	}
	u.Done("deleted %s (#%d) from %s", out.Slug, out.ID, env.Name)
	if out.UploadsRemoved {
		u.Done("removed %s", out.UploadsDir)
	}
	return &out, nil
}

func uploadsSafe(dir string) bool {
	return strings.Contains(dir, "/uploads/sites/") && !strings.Contains(dir, "..")
}

// Replace dumps selected tables on env, applies extra find/replace rules, and
// swaps them back in atomically (same as a migration onto itself).
func Replace(ctx context.Context, cfg *config.Config, u *ui.UI, envName string, sel config.Selection, opt Options) (*Result, error) {
	start := time.Now()
	sel.Normalize()
	sel.DB = true
	sel.Media, sel.Themes, sel.Plugins, sel.MuPlugins, sel.Files = false, nil, nil, false, nil
	applyConfirm(&opt)
	if opt.Parallel <= 0 {
		opt.Parallel = cfg.Parallel
	}
	if opt.Keep != 0 {
		cfg.Backups.Keep = opt.Keep
	}
	if len(sel.Replace) == 0 && len(sel.Regex) == 0 {
		return nil, fmt.Errorf("pass at least one --replace='old=>new' (or --regex)")
	}
	u.Step("Connecting to %s", envName)
	side, err := Connect(ctx, cfg, envName)
	if err != nil {
		return nil, err
	}
	res := &Result{Run: newRunID(), From: envName, To: envName, DryRun: opt.DryRun}
	p, err := plan.BuildRestore(side.Manifest, side.Manifest, sel, cfg, res.Run)
	if err != nil {
		return nil, err
	}
	res.Plan = p
	printPlan(u, side, side, p, sel, cfg, opt)
	if opt.DryRun {
		u.Println("%s", u.Dim("Dry run: nothing was changed."))
		res.OK = true
		return res, nil
	}
	if side.Env.Protected && opt.Confirm != side.Env.Name {
		if err := u.Confirm(u.Red(fmt.Sprintf("This rewrites tables on %s.", strings.ToUpper(side.Env.Name))), side.Env.Name); err != nil {
			return nil, err
		}
	}
	st := state.Load(cfg.Dir)
	if err := execute(ctx, cfg, u, source{side: side}, side, p, sel, opt, res, st, state.Key(envName, envName), nil); err != nil {
		return res, err
	}
	if opt.Verify {
		v, err := Verify(ctx, u, side, p)
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
