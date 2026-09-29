package migrate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/cloak-labs/wp-teleport/internal/agent"
	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/db"
	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

const snapshotMarker = "-- teleport-snapshot "

// SnapshotHeader is the first line of an export, so `teleport import` can map
// sites, prefixes and URLs onto any destination just like a live migration.
type SnapshotHeader struct {
	Version   int              `json:"version"`
	Env       string           `json:"env"`
	Created   int64            `json:"created"`
	Selection config.Selection `json:"selection"`
	Tables    []string         `json:"tables"`
	Manifest  *agent.Manifest  `json:"manifest"`
}

type ExportResult struct {
	File     string     `json:"file"`
	Bytes    int64      `json:"bytes"`
	Tables   int        `json:"tables"`
	DB       *db.Result `json:"db"`
	Duration float64    `json:"duration_seconds"`
}

// DefaultSnapshotName is <env>-<scope>-<timestamp>.sql.zst.
func DefaultSnapshotName(env string, sel config.Selection) string {
	scope := "site"
	switch {
	case sel.Network:
		scope = "network"
	case len(sel.Sites) > 0:
		scope = strings.Join(sel.Sites, "+")
	}
	return fmt.Sprintf("%s-%s-%s.sql.zst", env, scope, time.Now().Format("20060102-150405"))
}

// Export writes the selected tables of env to file (.sql, .sql.gz or .sql.zst;
// "-" for stdout).
func Export(ctx context.Context, cfg *config.Config, u *ui.UI, env string, sel config.Selection, file string) (*ExportResult, error) {
	start := time.Now()
	sel.DB = true
	if sel.HasFiles() {
		u.Warn("export only covers the database; files are ignored")
	}
	u.Step("Connecting to %s", env)
	src, err := Connect(ctx, cfg, env)
	if err != nil {
		return nil, err
	}
	p, err := plan.BuildExport(src.Manifest, sel, cfg)
	if err != nil {
		return nil, err
	}
	for _, w := range p.Warnings {
		u.Warn("%s", w)
	}
	if len(p.Tables) == 0 {
		return nil, fmt.Errorf("no tables selected")
	}
	hdr := SnapshotHeader{Version: 1, Env: env, Created: time.Now().Unix(), Selection: sel, Manifest: src.Manifest}
	m := *src.Manifest
	m.Runs, m.Lock, m.Tables = nil, nil, nil
	var bytes int64
	for _, t := range p.Tables {
		hdr.Tables = append(hdr.Tables, t.Src)
		if tt, ok := src.Manifest.Table(t.Src); ok {
			m.Tables = append(m.Tables, tt)
		}
		bytes += t.Bytes
	}
	hdr.Manifest = &m

	run := newRunID()
	var begin agent.BeginResult
	if err := agent.Call(ctx, src.Env, "begin", map[string]any{"cnf": run}, &begin); err != nil {
		return nil, err
	}
	defer abort(src, run, nil, false)

	var sink io.Writer = os.Stdout
	var f *os.File
	tmp := ""
	if file != "-" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, err
		}
		tmp = file + ".partial"
		if f, err = os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err != nil {
			return nil, err
		}
		defer func() {
			if f != nil {
				f.Close()
				os.Remove(tmp)
			}
		}()
		sink = f
	}
	bw := bufio.NewWriterSize(sink, 1<<20)
	var w io.Writer = bw
	var closer io.Closer
	switch {
	case strings.HasSuffix(file, ".zst"):
		zw, err := zstd.NewWriter(bw, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return nil, err
		}
		w, closer = zw, zw
	case strings.HasSuffix(file, ".gz"):
		gw := gzip.NewWriter(bw)
		w, closer = gw, gw
	}
	line, _ := json.Marshal(hdr)
	if _, err := fmt.Fprintf(w, "%s%s\n", snapshotMarker, line); err != nil {
		return nil, err
	}

	u.Step("Exporting %d tables (~%s) from %s", len(p.Tables), ui.Bytes(bytes), env)
	meter := u.Meter("export", 0)
	srcEP := db.Endpoint{Env: src.Env, Tools: src.Tools, Engine: src.Manifest.DB.Engine, DBName: src.Manifest.DB.Name, Cnf: begin.Cnf}
	dres, err := db.Export(ctx, srcEP, p.Tables, w, meter)
	meter.Stop()
	if err != nil {
		return nil, err
	}
	if closer != nil {
		if err := closer.Close(); err != nil {
			return nil, err
		}
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	res := &ExportResult{File: file, Tables: dres.Tables, DB: dres}
	if f != nil {
		if err := f.Close(); err != nil {
			return nil, err
		}
		f = nil
		if err := os.Rename(tmp, file); err != nil {
			return nil, err
		}
		if st, err := os.Stat(file); err == nil {
			res.Bytes = st.Size()
		}
	}
	res.Duration = time.Since(start).Seconds()
	return res, nil
}

// OpenSnapshot opens an export (plain, gzip or zstd) and reads its header.
func OpenSnapshot(file string) (*SnapshotHeader, io.Reader, func(), error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	magic, _ := br.Peek(4)
	var r io.Reader = br
	closeAll := func() { f.Close() }
	switch {
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, nil, err
		}
		r = zr
		closeAll = func() { zr.Close(); f.Close() }
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gr, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, nil, err
		}
		r = gr
	}
	body := bufio.NewReaderSize(r, 1<<20)
	first, err := body.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, snapshotMarker) {
		closeAll()
		return nil, nil, nil, fmt.Errorf("%s is not a teleport export (use `wp db import` for other SQL files)", file)
	}
	var hdr SnapshotHeader
	if err := json.Unmarshal([]byte(strings.TrimPrefix(first, snapshotMarker)), &hdr); err != nil {
		closeAll()
		return nil, nil, nil, fmt.Errorf("%s: bad snapshot header: %w", file, err)
	}
	if hdr.Manifest == nil {
		closeAll()
		return nil, nil, nil, fmt.Errorf("%s: snapshot header has no manifest", file)
	}
	hdr.Manifest.AssignSlugs()
	return &hdr, body, closeAll, nil
}

// Import restores a snapshot into env through temporary tables and an atomic
// swap. override narrows or redirects the snapshot's selection (--sites, --as,
// --create-site, extra --replace rules).
func Import(ctx context.Context, cfg *config.Config, u *ui.UI, file, to string, override config.Selection, opt Options) (*Result, error) {
	start := time.Now()
	hdr, body, closeFn, err := OpenSnapshot(file)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	sel := hdr.Selection
	if len(override.Sites) > 0 {
		sel.Sites, sel.Network = override.Sites, false
	}
	if len(override.Tables) > 0 {
		sel.Tables = override.Tables
	}
	sel.ExcludeTables = append(sel.ExcludeTables, override.ExcludeTables...)
	sel.As, sel.CreateSite, sel.SkipGUIDs = override.As, override.CreateSite, override.SkipGUIDs
	sel.Replace = append(sel.Replace, override.Replace...)
	sel.Regex = append(sel.Regex, override.Regex...)
	sel.Preserve = append(sel.Preserve, override.Preserve...)
	sel.ExcludePostTypes, sel.ExcludeSpam, sel.ExcludeTransients = nil, false, false
	sel.Media, sel.MediaSince, sel.Themes, sel.Plugins, sel.MuPlugins, sel.Files = false, "", nil, nil, false, nil
	sel.DB = true
	sel.Normalize()

	applyConfirm(&opt)
	if opt.Keep != 0 {
		cfg.Backups.Keep = opt.Keep
	}
	u.Step("Connecting to %s", to)
	dst, err := Connect(ctx, cfg, to)
	if err != nil {
		return nil, err
	}
	srcSide := &Side{Env: transport.New(&config.Env{Name: filepath.Base(file)}), Manifest: hdr.Manifest}
	srcSide.Env.Kind = transport.KindSnapshot
	res := &Result{Run: newRunID(), From: filepath.Base(file), To: to, DryRun: opt.DryRun}

	build := func() (*plan.Plan, error) {
		p, err := plan.BuildRestore(hdr.Manifest, dst.Manifest, sel, cfg, res.Run)
		if err != nil {
			return nil, err
		}
		inFile := map[string]bool{}
		for _, t := range hdr.Tables {
			inFile[t] = true
		}
		kept := p.Tables[:0]
		for _, t := range p.Tables {
			if inFile[t.Src] {
				kept = append(kept, t)
			}
		}
		p.Tables = kept
		return p, nil
	}
	p, err := build()
	if err != nil {
		return nil, err
	}
	res.Plan = p
	u.Println("%s", u.Dim(fmt.Sprintf("snapshot of %s taken %s", hdr.Env, time.Unix(hdr.Created, 0).Format("2006-01-02 15:04"))))
	printPlan(u, srcSide, dst, p, sel, cfg, opt)
	if opt.DryRun {
		u.Println("%s", u.Dim("Dry run: nothing was changed."))
		res.Duration = time.Since(start).Seconds()
		res.OK = true
		return res, nil
	}
	if len(p.Tables) == 0 && len(p.NeedsCreate()) == 0 {
		return nil, fmt.Errorf("the snapshot contains none of the selected tables")
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
		if p, err = build(); err != nil {
			return nil, err
		}
		res.Plan = p
	}
	if err := execute(ctx, cfg, u, source{snap: body, name: filepath.Base(file)}, dst, p, sel, opt, res, nil, "", nil); err != nil {
		return res, err
	}
	res.Duration = time.Since(start).Seconds()
	res.OK = true
	return res, nil
}
