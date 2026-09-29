// Package db streams tables between environments:
// mysqldump | zstd -> ssh -> rewrite -> zstd -> ssh -> mysql, one or more
// streams in parallel, always into temporary tables.
package db

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/sqlstream"
	"github.com/cloak-labs/wp-teleport/internal/transport"
)

// Endpoint is one side of a transfer.
type Endpoint struct {
	Env    *transport.Env
	Tools  transport.Tools
	Engine string
	DBName string
	Cnf    string
}

// Compress reports whether this leg should carry zstd frames. Local and
// Docker legs are pipes on this machine, so compression only costs CPU.
func (e Endpoint) Compress() bool {
	return e.Env.Kind == transport.KindSSH && e.Tools.Has("zstd")
}

func (e Endpoint) dumpCmd(tables []string, where string) (string, error) {
	bin := e.Tools.Dump(e.Engine)
	if bin == "" {
		return "", fmt.Errorf("%s: neither mysqldump nor mariadb-dump is installed", e.Env.Name)
	}
	args := []string{
		bin,
		"--defaults-extra-file=" + transport.Quote(e.Cnf),
		"--single-transaction", "--quick", "--skip-lock-tables", "--skip-add-locks",
		"--no-tablespaces", "--skip-triggers", "--add-drop-table",
		"--default-character-set=utf8mb4", "--max-allowed-packet=1G",
	}
	if !e.Tools.DumpIsMariaDB() && e.Engine == "mysql" {
		args = append(args, "--set-gtid-purged=OFF")
	}
	if where != "" {
		args = append(args, transport.Quote("--where="+where))
	}
	args = append(args, transport.Quote(e.DBName))
	for _, t := range tables {
		args = append(args, transport.Quote(t))
	}
	cmd := strings.Join(args, " ")
	if e.Compress() {
		cmd += " | zstd -T0 -3 -q -c"
	}
	return cmd, nil
}

func (e Endpoint) importCmd() (string, error) {
	bin := e.Tools.Client(e.Engine)
	if bin == "" {
		return "", fmt.Errorf("%s: the mysql client is not installed", e.Env.Name)
	}
	cmd := bin + " --defaults-extra-file=" + transport.Quote(e.Cnf) +
		" --default-character-set=utf8mb4 --max-allowed-packet=1G " + transport.Quote(e.DBName)
	if e.Compress() {
		cmd = "zstd -d -q -c | " + cmd
	}
	return cmd, nil
}

const header = "SET SESSION foreign_key_checks=0;\nSET SESSION unique_checks=0;\nSET SESSION autocommit=0;\nSET NAMES utf8mb4;\n"
const footer = "\nCOMMIT;\n"

// Stream is one mysqldump invocation.
type Stream struct {
	Tables []plan.TableJob
	Where  string
}

// Streams groups jobs into at most n streams, balanced by size. Tables with a
// --where filter need their own mysqldump call.
func Streams(jobs []plan.TableJob, n int) []Stream {
	if n < 1 {
		n = 1
	}
	var out []Stream
	var plain []plan.TableJob
	for _, j := range jobs {
		if j.Unchanged {
			continue
		}
		if j.Where != "" {
			out = append(out, Stream{Tables: []plan.TableJob{j}, Where: j.Where})
		} else {
			plain = append(plain, j)
		}
	}
	sort.SliceStable(plain, func(a, b int) bool { return plain[a].Bytes > plain[b].Bytes })
	bins := make([]Stream, min(n, len(plain)))
	sizes := make([]int64, len(bins))
	for _, j := range plain {
		k := 0
		for i := range sizes {
			if sizes[i] < sizes[k] {
				k = i
			}
		}
		bins[k].Tables = append(bins[k].Tables, j)
		sizes[k] += j.Bytes + 1
	}
	return append(bins, out...)
}

// Progress receives live counters.
type Progress interface {
	Add(n int64)
	SetExtra(s string)
}

// Result summarizes a transfer.
type Result struct {
	Streams int             `json:"streams"`
	Tables  int             `json:"tables"`
	Stats   sqlstream.Stats `json:"stats"`
}

// Transfer copies the jobs into temporary tables on dst.
func Transfer(ctx context.Context, src, dst Endpoint, jobs []plan.TableJob, base sqlstream.Options, parallel int, progress Progress) (*Result, error) {
	streams := Streams(jobs, parallel)
	res := &Result{Streams: len(streams)}
	total := 0
	for _, s := range streams {
		total += len(s.Tables)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		done     atomic.Int64
	)
	sem := make(chan struct{}, max(parallel, 1))
	for _, s := range streams {
		wg.Add(1)
		go func(s Stream) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			stats, err := runStream(ctx, src, dst, s, base, progress)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			res.Tables += len(s.Tables)
			res.Stats.InBytes += stats.InBytes
			res.Stats.OutBytes += stats.OutBytes
			res.Stats.InsertLines += stats.InsertLines
			res.Stats.ChangedValues += stats.ChangedValues
			res.Stats.Tables += stats.Tables
			n := done.Add(int64(len(s.Tables)))
			if progress != nil {
				progress.SetExtra(fmt.Sprintf("tables %d/%d", n, total))
			}
		}(s)
	}
	wg.Wait()
	if firstErr != nil {
		return res, firstErr
	}
	return res, nil
}

func runStream(ctx context.Context, src, dst Endpoint, s Stream, base sqlstream.Options, progress Progress) (sqlstream.Stats, error) {
	var names []string
	rename := map[string]string{}
	for _, t := range s.Tables {
		names = append(names, t.Src)
		rename[t.Src] = t.Tmp
	}
	opt := base
	opt.Rename = rename

	dumpScript, err := src.dumpCmd(names, s.Where)
	if err != nil {
		return sqlstream.Stats{}, err
	}
	importScript, err := dst.importCmd()
	if err != nil {
		return sqlstream.Stats{}, err
	}
	label := names[0]
	if len(names) > 1 {
		label = fmt.Sprintf("%s (+%d)", names[0], len(names)-1)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srcCmd := src.Env.Command(ctx, dumpScript)
	dstCmd := dst.Env.Command(ctx, importScript)
	var srcErr, dstErr, dstOut bytes.Buffer
	srcCmd.Stderr = &srcErr
	dstCmd.Stderr = &dstErr
	dstCmd.Stdout = &dstOut
	srcOut, err := srcCmd.StdoutPipe()
	if err != nil {
		return sqlstream.Stats{}, err
	}
	dstIn, err := dstCmd.StdinPipe()
	if err != nil {
		return sqlstream.Stats{}, err
	}
	if err := dstCmd.Start(); err != nil {
		return sqlstream.Stats{}, err
	}
	if err := srcCmd.Start(); err != nil {
		dstIn.Close()
		dstCmd.Wait()
		return sqlstream.Stats{}, err
	}

	var r io.Reader = srcOut
	if progress != nil {
		r = io.TeeReader(r, progressWriter{progress})
	}
	if src.Compress() {
		zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(2))
		if err != nil {
			return sqlstream.Stats{}, err
		}
		defer zr.Close()
		r = zr
	}
	var w io.Writer = dstIn
	var zw *zstd.Encoder
	if dst.Compress() {
		zw, err = zstd.NewWriter(dstIn, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(2))
		if err != nil {
			return sqlstream.Stats{}, err
		}
		w = zw
	}

	stats, rwErr := func() (sqlstream.Stats, error) {
		if _, err := io.WriteString(w, header); err != nil {
			return sqlstream.Stats{}, err
		}
		st, err := sqlstream.Rewrite(r, w, opt)
		if err != nil {
			return st, err
		}
		_, err = io.WriteString(w, footer)
		return st, err
	}()
	if zw != nil {
		if err := zw.Close(); err != nil && rwErr == nil {
			rwErr = err
		}
	}
	dstIn.Close()
	if rwErr != nil {
		cancel()
	}
	srcWait := srcCmd.Wait()
	dstWait := dstCmd.Wait()

	switch {
	case srcWait != nil && ctx.Err() == nil:
		return stats, fmt.Errorf("dump of %s on %s failed: %v\n%s", label, src.Env.Name, srcWait, strings.TrimSpace(srcErr.String()))
	case dstWait != nil:
		msg := strings.TrimSpace(dstErr.String() + dstOut.String())
		return stats, fmt.Errorf("import of %s on %s failed: %v\n%s", label, dst.Env.Name, dstWait, msg)
	case rwErr != nil:
		return stats, fmt.Errorf("streaming %s: %w", label, rwErr)
	case !stats.Completed:
		return stats, fmt.Errorf("dump of %s on %s ended early (no completion marker)\n%s", label, src.Env.Name, strings.TrimSpace(srcErr.String()))
	}
	return stats, nil
}

type progressWriter struct{ p Progress }

func (w progressWriter) Write(b []byte) (int, error) {
	w.p.Add(int64(len(b)))
	return len(b), nil
}
