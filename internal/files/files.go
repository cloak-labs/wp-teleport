// Package files moves wp-content files. It picks the fastest strategy per job:
//
//   - tar+zstd stream when the destination directory is empty (no per-file
//     round trips),
//   - rsync from this machine for local<->remote deltas, split into parallel
//     shards,
//   - rsync run on the source server over `ssh -A` when both ends are remote
//     (files never pass through this machine),
//   - a tar relay through this machine as the fallback.
package files

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/cloak-labs/wp-teleport/internal/config"
	"github.com/cloak-labs/wp-teleport/internal/plan"
	"github.com/cloak-labs/wp-teleport/internal/transport"
	"github.com/cloak-labs/wp-teleport/internal/ui"
)

type Side struct {
	Env   *transport.Env
	Tools transport.Tools
}

type Options struct {
	Parallel int
	DryRun   bool
	Rsync    transport.RsyncInfo
	UI       *ui.UI
}

type Result struct {
	Label    string `json:"label"`
	Strategy string `json:"strategy"`
	Files    int64  `json:"files"`
	Bytes    int64  `json:"bytes"`
	Note     string `json:"note,omitempty"`
}

// hostEnv runs commands on this machine.
var hostEnv = transport.New(&config.Env{Name: "this machine", Path: "/"})

// Transfer runs one job.
func Transfer(ctx context.Context, src, dst Side, job plan.FileJob, opt Options) (Result, error) {
	res := Result{Label: job.Label}
	srcHost := src.Env.HostDir(job.Src)
	dstHost := dst.Env.HostDir(job.Dst)

	exists, err := dirHasFiles(ctx, src.Env, srcHost, job.Src)
	if err != nil {
		return res, err
	}
	if !exists {
		res.Strategy = "skip"
		res.Note = "source directory is empty or missing"
		return res, nil
	}

	dstEmpty := false
	if has, err := dirHasFiles(ctx, dst.Env, dstHost, job.Dst); err == nil && !has {
		dstEmpty = true
	}
	hostRsync := opt.Rsync.Path != ""

	bothRemote := src.Env.Kind == transport.KindSSH && dst.Env.Kind == transport.KindSSH
	if bothRemote && src.Tools.Has("rsync") {
		if host, ok := directReachable(ctx, src, dst); ok {
			res.Strategy = "rsync (server to server)"
			return directRsyncJob(ctx, src, dst, host, job, opt, res)
		}
		res.Note = "servers cannot reach each other over ssh -A; relaying through this machine"
	}

	switch {
	case dstEmpty && !job.Delete:
		res.Strategy = "tar stream"
		return tarStream(ctx, src, dst, srcHost, dstHost, job, opt, res)
	case (srcHost != "" || src.Env.Kind == transport.KindSSH) && (dstHost != "" || dst.Env.Kind == transport.KindSSH) &&
		!(srcHost == "" && dstHost == "") && hostRsync:
		res.Strategy = "rsync"
		if !opt.Rsync.Modern() {
			res.Note = "slow rsync (" + opt.Rsync.Path + "); " + transport.InstallHint() + " for zstd + progress"
		}
		return hostRsyncJob(ctx, src, dst, srcHost, dstHost, job, opt, res)
	}
	res.Strategy = "tar relay"
	if job.Delete {
		res.Note = strings.TrimSpace(res.Note + " (--delete is not supported by the tar relay)")
	}
	return tarStream(ctx, src, dst, srcHost, dstHost, job, opt, res)
}

func dirHasFiles(ctx context.Context, env *transport.Env, hostPath, path string) (bool, error) {
	if hostPath != "" {
		entries, err := os.ReadDir(hostPath)
		if os.IsNotExist(err) {
			return false, nil
		}
		return len(entries) > 0, err
	}
	out, err := env.Run(ctx, "if [ -d "+transport.Quote(path)+" ] && [ -n \"$(ls -A "+transport.Quote(path)+" 2>/dev/null)\" ]; then echo yes; fi", nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// --- tar -------------------------------------------------------------------

func tarExcludes(patterns []string) string {
	var parts []string
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "/")
		if strings.HasPrefix(p, "/") {
			p = "./" + strings.TrimPrefix(p, "/")
		}
		parts = append(parts, transport.Quote("--exclude="+p))
	}
	return strings.Join(parts, " ")
}

func tarCreate(env *transport.Env, root string, job plan.FileJob, compress bool) string {
	flags := ""
	if env.Kind == transport.KindLocal && runtime.GOOS == "darwin" {
		flags = "COPYFILE_DISABLE=1 "
	}
	cmd := "cd " + transport.Quote(root) + " && "
	tar := flags + "tar"
	if env.Kind == transport.KindLocal && runtime.GOOS == "darwin" {
		tar += " --no-mac-metadata --no-xattrs"
	}
	if job.Since != "" {
		cmd += "find . -type f -newermt " + transport.Quote(sinceDate(job.Since)) + " | " + tar + " " + tarExcludes(job.Exclude) + " -cf - -T -"
	} else {
		cmd += tar + " " + tarExcludes(job.Exclude) + " -cf - ."
	}
	if compress {
		cmd += " | zstd -T0 -3 -q -c"
	}
	return cmd
}

func tarExtract(root string, compress bool) string {
	cmd := "mkdir -p " + transport.Quote(root) + " && "
	if compress {
		cmd += "zstd -d -q -c | "
	}
	return cmd + "tar -C " + transport.Quote(root) + " --no-same-owner -xf -"
}

func tarStream(ctx context.Context, src, dst Side, srcHost, dstHost string, job plan.FileJob, opt Options, res Result) (Result, error) {
	srcEnv, srcRoot := src.Env, job.Src
	if srcHost != "" {
		srcEnv, srcRoot = hostEnv, srcHost
	}
	dstEnv, dstRoot := dst.Env, job.Dst
	if dstHost != "" {
		dstEnv, dstRoot = hostEnv, dstHost
	}
	if opt.DryRun {
		n, b, err := measure(ctx, srcEnv, srcRoot, job)
		res.Files, res.Bytes = n, b
		return res, err
	}
	srcZ := srcEnv.Kind == transport.KindSSH && src.Tools.Has("zstd")
	dstZ := dstEnv.Kind == transport.KindSSH && dst.Tools.Has("zstd")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := srcEnv.Command(ctx, tarCreate(srcEnv, srcRoot, job, srcZ))
	dc := dstEnv.Command(ctx, tarExtract(dstRoot, dstZ))
	var se, de bytes.Buffer
	sc.Stderr, dc.Stderr = &se, &de
	out, err := sc.StdoutPipe()
	if err != nil {
		return res, err
	}
	in, err := dc.StdinPipe()
	if err != nil {
		return res, err
	}
	if err := dc.Start(); err != nil {
		return res, err
	}
	if err := sc.Start(); err != nil {
		in.Close()
		dc.Wait()
		return res, err
	}
	meter := opt.UI.Meter(job.Label, 0)
	var r io.Reader = io.TeeReader(out, meter)
	var w io.WriteCloser = in
	switch {
	case srcZ && !dstZ:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return res, err
		}
		defer zr.Close()
		r = zr
	case !srcZ && dstZ:
		zw, err := zstd.NewWriter(in, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			return res, err
		}
		w = &encoderCloser{zw, in}
	}
	_, copyErr := io.Copy(w, r)
	closeErr := w.Close()
	if w != in {
		in.Close()
	}
	if copyErr != nil {
		cancel()
	}
	sErr, dErr := sc.Wait(), dc.Wait()
	meter.Stop()
	res.Bytes = meter.Bytes()
	switch {
	case dErr != nil:
		return res, fmt.Errorf("%s: extract on %s failed: %v\n%s", job.Label, dstEnv.Name, dErr, strings.TrimSpace(de.String()))
	case sErr != nil:
		return res, fmt.Errorf("%s: archive on %s failed: %v\n%s", job.Label, srcEnv.Name, sErr, strings.TrimSpace(se.String()))
	case copyErr != nil:
		return res, copyErr
	case closeErr != nil:
		return res, closeErr
	}
	res.Note = strings.TrimSpace(res.Note + " " + ui.Bytes(res.Bytes) + " on the wire")
	return res, nil
}

type encoderCloser struct {
	*zstd.Encoder
	under io.Closer
}

func (e *encoderCloser) Close() error {
	err := e.Encoder.Close()
	if cerr := e.under.Close(); err == nil {
		err = cerr
	}
	return err
}

func measure(ctx context.Context, env *transport.Env, root string, job plan.FileJob) (int64, int64, error) {
	filter := ""
	if job.Since != "" {
		filter = " -newermt " + transport.Quote(sinceDate(job.Since))
	}
	out, err := env.Run(ctx, "cd "+transport.Quote(root)+" && find . -type f"+filter+" -exec ls -ln {} + 2>/dev/null | awk '{n++; b+=$5} END {print n+0, b+0}'", nil)
	if err != nil {
		return 0, 0, err
	}
	var n, b int64
	fmt.Sscan(strings.TrimSpace(string(out)), &n, &b)
	return n, b, nil
}

func sinceDate(s string) string {
	if len(s) == 7 {
		return s + "-01"
	}
	if len(s) == 4 {
		return s + "-01-01"
	}
	return s
}

// --- rsync -------------------------------------------------------------------

type rsyncRunner func(ctx context.Context, srcRel, dstRel string, args []string, filesFrom []byte, showProgress bool) (Result, error)

func rsyncBaseArgs(job plan.FileJob, modern, remote bool, remoteModern bool, dryRun bool) []string {
	args := []string{"-rlt", "--partial", "--stats"}
	if modern {
		args = append(args, "--info=progress2")
	}
	if remote {
		if modern && remoteModern {
			args = append(args, "-z", "--compress-choice=zstd", "--compress-level=3")
		} else {
			args = append(args, "-z")
		}
	}
	if job.Delete {
		args = append(args, "--delete")
	}
	if dryRun {
		args = append(args, "-n")
	}
	for _, e := range job.Exclude {
		args = append(args, "--exclude="+e)
	}
	return args
}

func hostRsyncJob(ctx context.Context, src, dst Side, srcHost, dstHost string, job plan.FileJob, opt Options, res Result) (Result, error) {
	remote := src.Env.Kind == transport.KindSSH && srcHost == "" || dst.Env.Kind == transport.KindSSH && dstHost == ""
	remoteSide := src
	if dst.Env.Kind == transport.KindSSH && dstHost == "" {
		remoteSide = dst
	}
	remoteModern := transport.ParseRsync("", remoteSide.Tools.RsyncVersion).Modern()
	base := rsyncBaseArgs(job, opt.Rsync.Modern(), remote, remoteModern, opt.DryRun)
	if remote {
		shell, err := remoteSide.Env.RsyncShell()
		if err != nil {
			return res, err
		}
		base = append(base, "-e", shell)
	}
	spec := func(side Side, host, path string) (string, error) {
		if host != "" {
			return host, nil
		}
		return side.Env.RemoteSpec(path)
	}
	srcRoot, err := spec(src, srcHost, job.Src)
	if err != nil {
		return res, err
	}
	dstRoot, err := spec(dst, dstHost, job.Dst)
	if err != nil {
		return res, err
	}
	if !opt.DryRun {
		if dstHost != "" {
			os.MkdirAll(dstHost, 0o755)
		} else if _, err := dst.Env.Run(ctx, "mkdir -p "+transport.Quote(job.Dst), nil); err != nil {
			return res, err
		}
	}
	run := func(ctx context.Context, srcRel, dstRel string, args []string, filesFrom []byte, show bool) (Result, error) {
		full := append(append([]string{}, base...), args...)
		if filesFrom != nil {
			full = append(full, "--files-from=-")
		}
		full = append(full, joinRel(srcRoot, srcRel)+"/", joinRel(dstRoot, dstRel)+"/")
		cmd := exec.CommandContext(ctx, opt.Rsync.Path, full...)
		if filesFrom != nil {
			cmd.Stdin = bytes.NewReader(filesFrom)
		}
		return execRsync(cmd, show && !opt.DryRun, opt.UI)
	}
	return shardedRsync(ctx, src.Env, srcHost, job, opt, res, run)
}

func joinRel(root, rel string) string {
	root = strings.TrimRight(root, "/")
	if rel == "" || rel == "." {
		return root
	}
	return root + "/" + strings.Trim(rel, "/")
}

// directReachable checks whether src can ssh to dst with our forwarded agent.
func directReachable(ctx context.Context, src, dst Side) (string, bool) {
	si, err := src.Env.ResolveSSH()
	if err != nil {
		return "", false
	}
	di, err := dst.Env.ResolveSSH()
	if err != nil {
		return "", false
	}
	host := di.Hostname
	if host == si.Hostname {
		host = "127.0.0.1"
	}
	port := ""
	if di.Port != "" && di.Port != "22" {
		port = " -p " + di.Port
	}
	probe := "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5" + port + " " + transport.Quote(di.User+"@"+host) + " true"
	if _, err := src.Env.RunForwardAgent(ctx, probe); err != nil {
		return "", false
	}
	return host, true
}

func directRsyncJob(ctx context.Context, src, dst Side, host string, job plan.FileJob, opt Options, res Result) (Result, error) {
	di, _ := dst.Env.ResolveSSH()
	srcModern := transport.ParseRsync("", src.Tools.RsyncVersion).Modern()
	dstModern := transport.ParseRsync("", dst.Tools.RsyncVersion).Modern()
	remote := host != "127.0.0.1"
	base := rsyncBaseArgs(job, srcModern, remote, dstModern, opt.DryRun)
	sshCmd := "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new"
	if di.Port != "" && di.Port != "22" {
		sshCmd += " -p " + di.Port
	}
	base = append(base, "-e", sshCmd)
	if !opt.DryRun {
		if _, err := dst.Env.Run(ctx, "mkdir -p "+transport.Quote(job.Dst), nil); err != nil {
			return res, err
		}
	}
	dstRoot := di.User + "@" + host + ":" + job.Dst
	run := func(ctx context.Context, srcRel, dstRel string, args []string, filesFrom []byte, show bool) (Result, error) {
		full := append(append([]string{"rsync"}, base...), args...)
		var quoted []string
		for _, a := range full {
			quoted = append(quoted, transport.Quote(a))
		}
		script := strings.Join(quoted, " ")
		if filesFrom != nil {
			script += " --files-from=-"
		}
		script += " " + transport.Quote(joinRel(job.Src, srcRel)+"/") + " " + transport.Quote(joinRel(dstRoot, dstRel)+"/")
		cmd := src.Env.CommandForwardAgent(ctx, script)
		if filesFrom != nil {
			cmd.Stdin = bytes.NewReader(filesFrom)
		}
		return execRsync(cmd, show && !opt.DryRun, opt.UI)
	}
	return shardedRsync(ctx, src.Env, "", job, opt, res, run)
}

// shardedRsync splits a job into top-level directories run in parallel.
// --delete and --media-since runs stay single so deletions stay correct.
func shardedRsync(ctx context.Context, srcEnv *transport.Env, srcHost string, job plan.FileJob, opt Options, res Result, run rsyncRunner) (Result, error) {
	var filesFrom []byte
	if job.Since != "" {
		list, err := listSince(ctx, srcEnv, srcHost, job)
		if err != nil {
			return res, err
		}
		if len(list) == 0 {
			res.Note = "no files changed since " + job.Since
			return res, nil
		}
		filesFrom = list
	}
	if opt.Parallel <= 1 || job.Delete || filesFrom != nil || opt.DryRun {
		r, err := run(ctx, "", "", nil, filesFrom, true)
		r.Label, r.Strategy, r.Note = res.Label, res.Strategy, strings.TrimSpace(res.Note+" "+r.Note)
		return r, err
	}
	shards, err := listShards(ctx, srcEnv, srcHost, job)
	if err != nil || len(shards) < 2 {
		r, err := run(ctx, "", "", nil, nil, true)
		r.Label, r.Strategy, r.Note = res.Label, res.Strategy, strings.TrimSpace(res.Note+" "+r.Note)
		return r, err
	}
	res.Strategy += fmt.Sprintf(", %d shards x%d", len(shards), opt.Parallel)
	type task struct {
		rel  string
		args []string
	}
	var tasks []task
	var inner []string
	for _, e := range job.Exclude {
		if !strings.HasPrefix(e, "/") {
			inner = append(inner, "--exclude="+e)
		}
	}
	parents := map[string]bool{"": true}
	for _, s := range shards {
		tasks = append(tasks, task{rel: s, args: inner})
		if i := strings.LastIndex(s, "/"); i > 0 {
			parents[s[:i]] = true
		}
	}
	for p := range parents {
		// top-level files of the root (and of expanded parents) only
		tasks = append(tasks, task{rel: p, args: []string{"--exclude=*/"}})
	}
	meter := opt.UI.Meter(job.Label, 0)
	defer meter.Stop()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		done     int
	)
	sem := make(chan struct{}, opt.Parallel)
	for _, t := range tasks {
		wg.Add(1)
		go func(t task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := run(ctx, t.rel, t.rel, t.args, nil, false)
			mu.Lock()
			defer mu.Unlock()
			done++
			meter.SetExtra(fmt.Sprintf("shards %d/%d", done, len(tasks)))
			meter.Add(r.Bytes)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s/%s: %w", job.Label, t.rel, err)
			}
			res.Files += r.Files
			res.Bytes += r.Bytes
		}(t)
	}
	wg.Wait()
	return res, firstErr
}

func listShards(ctx context.Context, env *transport.Env, hostPath string, job plan.FileJob) ([]string, error) {
	excluded := map[string]bool{}
	for _, e := range job.Exclude {
		if strings.HasPrefix(e, "/") {
			excluded[strings.Trim(e, "/")] = true
		}
	}
	list := func(rel string) ([]string, error) {
		var names []string
		if hostPath != "" {
			entries, err := os.ReadDir(filepath.Join(hostPath, rel))
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.IsDir() {
					names = append(names, e.Name())
				}
			}
			return names, nil
		}
		out, err := env.Run(ctx, "cd "+transport.Quote(joinRel(job.Src, rel))+" && for d in */; do [ -d \"$d\" ] && [ ! -L \"${d%/}\" ] && echo \"${d%/}\"; done; true", nil)
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" && l != "*" {
				names = append(names, l)
			}
		}
		return names, nil
	}
	top, err := list("")
	if err != nil {
		return nil, err
	}
	var shards []string
	for _, d := range top {
		if excluded[d] {
			continue
		}
		if d == "sites" {
			sub, err := list("sites")
			if err != nil {
				return nil, err
			}
			for _, s := range sub {
				shards = append(shards, "sites/"+s)
			}
			continue
		}
		shards = append(shards, d)
	}
	return shards, nil
}

func listSince(ctx context.Context, env *transport.Env, hostPath string, job plan.FileJob) ([]byte, error) {
	script := "cd " + transport.Quote(job.Src) + " && find . -type f -newermt " + transport.Quote(sinceDate(job.Since))
	var out []byte
	var err error
	if hostPath != "" {
		out, err = hostEnv.Run(ctx, "cd "+transport.Quote(hostPath)+" && find . -type f -newermt "+transport.Quote(sinceDate(job.Since)), nil)
	} else {
		out, err = env.Run(ctx, script, nil)
	}
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimPrefix(strings.TrimSpace(l), "./")
		if l != "" && !excludedPath(l, job.Exclude) {
			buf.WriteString(l + "\n")
		}
	}
	return buf.Bytes(), nil
}

func excludedPath(rel string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(p, "/") {
			if strings.HasPrefix("/"+rel+"/", strings.TrimRight(p, "/")+"/") {
				return true
			}
			continue
		}
		base := filepath.Base(rel)
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

var (
	statFiles = regexp.MustCompile(`Number of (?:regular )?files transferred: ([\d,]+)`)
	statBytes = regexp.MustCompile(`Total transferred file size: ([\d,]+)`)
)

func execRsync(cmd *exec.Cmd, show bool, u *ui.UI) (Result, error) {
	var res Result
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, err
	}
	var all bytes.Buffer
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	sc.Split(scanCRLF)
	for sc.Scan() {
		line := sc.Text()
		all.WriteString(line + "\n")
		if show && strings.Contains(line, "%") && strings.Contains(line, "/s") {
			u.Progress(strings.TrimSpace(line))
		}
	}
	err = cmd.Wait()
	if show {
		u.Progress("")
	}
	if m := statFiles.FindStringSubmatch(all.String()); m != nil {
		res.Files, _ = strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	}
	if m := statBytes.FindStringSubmatch(all.String()); m != nil {
		res.Bytes, _ = strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	}
	if err != nil {
		// 24 = some files vanished during transfer; harmless for uploads.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 24 {
			res.Note = "some source files vanished during transfer"
			return res, nil
		}
		return res, fmt.Errorf("rsync failed: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return res, nil
}

// scanCRLF splits on \n or \r so rsync's progress2 updates arrive live.
func scanCRLF(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
