package db

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/sqlstream"
)

// Export dumps jobs from src into w as plain SQL, one stream at a time so the
// output is a single ordered file. Table names and values are left untouched.
func Export(ctx context.Context, src Endpoint, jobs []plan.TableJob, w io.Writer, progress Progress) (*Result, error) {
	streams := Streams(jobs, 1)
	res := &Result{Streams: len(streams)}
	if _, err := io.WriteString(w, header); err != nil {
		return res, err
	}
	for i, s := range streams {
		var names []string
		for _, t := range s.Tables {
			names = append(names, t.Src)
		}
		script, err := src.dumpCmd(names, s.Where)
		if err != nil {
			return res, err
		}
		cmd := src.Env.Command(ctx, script)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return res, err
		}
		if err := cmd.Start(); err != nil {
			return res, err
		}
		var r io.Reader = out
		if progress != nil {
			r = io.TeeReader(r, progressWriter{progress})
		}
		var zr *zstd.Decoder
		if src.Compress() {
			if zr, err = zstd.NewReader(r); err != nil {
				cmd.Wait()
				return res, err
			}
			r = zr
		}
		st, rwErr := sqlstream.Rewrite(r, w, sqlstream.Options{})
		if zr != nil {
			zr.Close()
		}
		waitErr := cmd.Wait()
		switch {
		case waitErr != nil:
			return res, fmt.Errorf("dump of %s on %s failed: %v\n%s", names[0], src.Env.Name, waitErr, strings.TrimSpace(stderr.String()))
		case rwErr != nil:
			return res, rwErr
		case !st.Completed:
			return res, fmt.Errorf("dump of %s on %s ended early", names[0], src.Env.Name)
		}
		res.Tables += len(s.Tables)
		res.Stats.InBytes += st.InBytes
		res.Stats.OutBytes += st.OutBytes
		res.Stats.Tables += st.Tables
		if progress != nil {
			progress.SetExtra(fmt.Sprintf("stream %d/%d", i+1, len(streams)))
		}
	}
	_, err := io.WriteString(w, footer)
	return res, err
}

// Import streams an exported SQL file into temporary tables on dst, applying
// the same renames and replacements as a live transfer.
func Import(ctx context.Context, r io.Reader, dst Endpoint, jobs []plan.TableJob, base sqlstream.Options, progress Progress) (*Result, error) {
	rename := map[string]string{}
	for _, t := range jobs {
		rename[t.Src] = t.Tmp
	}
	opt := base
	opt.Rename = rename
	opt.Only = rename

	script, err := dst.importCmd()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := dst.Env.Command(ctx, script)
	var stderr, stdout bytes.Buffer
	cmd.Stderr, cmd.Stdout = &stderr, &stdout
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if progress != nil {
		r = io.TeeReader(r, progressWriter{progress})
	}
	var w io.Writer = in
	var zw *zstd.Encoder
	if dst.Compress() {
		if zw, err = zstd.NewWriter(in, zstd.WithEncoderLevel(zstd.SpeedFastest)); err != nil {
			return nil, err
		}
		w = zw
	}
	st, rwErr := func() (sqlstream.Stats, error) {
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
	in.Close()
	if rwErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	res := &Result{Streams: 1, Tables: len(jobs), Stats: st}
	switch {
	case waitErr != nil:
		return res, fmt.Errorf("import on %s failed: %v\n%s", dst.Env.Name, waitErr, strings.TrimSpace(stderr.String()+stdout.String()))
	case rwErr != nil:
		return res, fmt.Errorf("reading snapshot: %w", rwErr)
	case !st.Completed:
		return res, fmt.Errorf("snapshot is truncated (no completion marker)")
	}
	return res, nil
}
