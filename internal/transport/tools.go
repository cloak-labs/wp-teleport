package transport

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Tools records which native binaries an environment has.
type Tools struct {
	Paths        map[string]string `json:"paths"`
	DumpVersion  string            `json:"dump_version"`
	RsyncVersion string            `json:"rsync_version"`
}

const toolsScript = `for c in mysqldump mariadb-dump mysql mariadb zstd rsync tar find; do p=$(command -v $c 2>/dev/null) && echo "path $c=$p"; done
d=$(command -v mariadb-dump 2>/dev/null || command -v mysqldump 2>/dev/null) && echo "dump $($d --version 2>/dev/null | head -n 1)"
command -v rsync >/dev/null 2>&1 && echo "rsync $(rsync --version 2>/dev/null | head -n 1)"
true`

// Tools probes the environment once (cached).
func (e *Env) Tools(ctx context.Context) (Tools, error) {
	e.toolsOnce.Do(func() {
		out, err := e.Run(ctx, toolsScript, nil)
		e.tools = parseTools(string(out))
		e.toolsErr = err
	})
	return e.tools, e.toolsErr
}

func parseTools(out string) Tools {
	t := Tools{Paths: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "path":
			if k, v, ok := strings.Cut(rest, "="); ok {
				t.Paths[k] = v
			}
		case "dump":
			t.DumpVersion = rest
		case "rsync":
			t.RsyncVersion = rest
		}
	}
	return t
}

func (t Tools) Has(name string) bool { return t.Paths[name] != "" }

// Dump is the dump binary to use, preferring the vendor-native one.
func (t Tools) Dump(engine string) string {
	if engine == "mariadb" && t.Has("mariadb-dump") {
		return "mariadb-dump"
	}
	if t.Has("mysqldump") {
		return "mysqldump"
	}
	if t.Has("mariadb-dump") {
		return "mariadb-dump"
	}
	return ""
}

func (t Tools) Client(engine string) string {
	if engine == "mariadb" && t.Has("mariadb") {
		return "mariadb"
	}
	if t.Has("mysql") {
		return "mysql"
	}
	if t.Has("mariadb") {
		return "mariadb"
	}
	return ""
}

// DumpIsMariaDB reports whether the dump client is MariaDB's (which lacks
// MySQL-only flags such as --set-gtid-purged).
func (t Tools) DumpIsMariaDB() bool {
	return strings.Contains(strings.ToLower(t.DumpVersion), "mariadb")
}

// RsyncInfo describes an rsync binary.
type RsyncInfo struct {
	Path      string
	Protocol  int
	Major     int
	Minor     int
	Patch     int
	OpenRsync bool
}

// Modern reports rsync >= 3.2.3 (zstd, --mkpath, progress2).
func (r RsyncInfo) Modern() bool {
	return !r.OpenRsync && (r.Major > 3 || r.Major == 3 && (r.Minor > 2 || r.Minor == 2 && r.Patch >= 3))
}

var rsyncVer = regexp.MustCompile(`version (\d+)\.(\d+)\.(\d+)`)
var rsyncProto = regexp.MustCompile(`protocol version (\d+)`)

// ParseRsync reads `rsync --version` output.
func ParseRsync(path, version string) RsyncInfo {
	r := RsyncInfo{Path: path}
	if strings.Contains(version, "openrsync") {
		r.OpenRsync = true
	}
	if m := rsyncVer.FindStringSubmatch(version); m != nil && !r.OpenRsync {
		r.Major, _ = strconv.Atoi(m[1])
		r.Minor, _ = strconv.Atoi(m[2])
		r.Patch, _ = strconv.Atoi(m[3])
	}
	if m := rsyncProto.FindStringSubmatch(version); m != nil {
		r.Protocol, _ = strconv.Atoi(m[1])
	}
	return r
}

// HostRsync finds the best rsync on this machine, preferring Homebrew's over
// macOS's bundled openrsync.
func HostRsync() RsyncInfo {
	candidates := []string{"/opt/homebrew/bin/rsync", "/usr/local/bin/rsync"}
	if p, err := exec.LookPath("rsync"); err == nil {
		candidates = append(candidates, p)
	}
	var best RsyncInfo
	for _, c := range candidates {
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		info := ParseRsync(c, string(out))
		if info.Modern() {
			return info
		}
		if best.Path == "" {
			best = info
		}
	}
	return best
}

// InstallHint says how to get a modern rsync on this OS.
func InstallHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install rsync"
	}
	return "install rsync >= 3.2.3 with your package manager"
}
