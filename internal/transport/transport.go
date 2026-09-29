// Package transport runs shell commands inside an environment: over SSH
// (with connection multiplexing), inside a Docker container, or on this machine.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/cloak-labs/wp-teleport/internal/config"
)

type Kind string

const (
	KindSSH    Kind = "ssh"
	KindDocker Kind = "docker"
	KindLocal  Kind = "local"
	// KindSnapshot labels a snapshot file standing in for a source environment.
	KindSnapshot Kind = "snapshot"
)

// Env is a live handle on one configured environment.
type Env struct {
	*config.Env
	Kind Kind

	sshOnce sync.Once
	sshInfo SSHInfo
	sshErr  error

	mountOnce sync.Once
	mounts    []mount

	toolsOnce sync.Once
	tools     Tools
	toolsErr  error
}

// SSHInfo is the resolved connection target from `ssh -G`.
type SSHInfo struct {
	User       string
	Hostname   string
	Port       string
	Identities []string
}

func New(c *config.Env) *Env {
	e := &Env{Env: c, Kind: KindLocal}
	switch {
	case c.SSH != "":
		e.Kind = KindSSH
	case c.Docker != "":
		e.Kind = KindDocker
	}
	return e
}

// Label is a short human description, e.g. "production (ssh wp)".
func (e *Env) Label() string {
	switch e.Kind {
	case KindSSH:
		return fmt.Sprintf("%s (ssh %s)", e.Name, e.SSH)
	case KindDocker:
		return fmt.Sprintf("%s (docker %s)", e.Name, e.Docker)
	case KindSnapshot:
		return fmt.Sprintf("%s (snapshot)", e.Name)
	}
	return fmt.Sprintf("%s (local %s)", e.Name, e.Path)
}

// WPCmd is the shell-ready WP-CLI invocation for this environment.
func (e *Env) WPCmd() string {
	parts := []string{Quote(e.WP)}
	for _, f := range e.WPFlags {
		parts = append(parts, Quote(f))
	}
	return strings.Join(parts, " ")
}

// Command builds a command that runs script from the WordPress root.
func (e *Env) Command(ctx context.Context, script string) *exec.Cmd {
	full := "cd " + quotePath(e.Path) + " && " + script
	switch e.Kind {
	case KindSSH:
		args := append(SSHOptions(), "-T", e.sshTarget())
		args = append(args, e.sshPortArgs()...)
		args = append(args, "--", full)
		return exec.CommandContext(ctx, "ssh", args...)
	case KindDocker:
		return exec.CommandContext(ctx, "docker", "exec", "-i", e.Docker, "sh", "-c", full)
	}
	return exec.CommandContext(ctx, "sh", "-c", full)
}

// Interactive runs script from the WordPress root attached to this terminal.
// With tty, a pseudo-terminal is allocated (for `wp shell`, a login shell, ...).
func (e *Env) Interactive(ctx context.Context, script string, tty bool) *exec.Cmd {
	full := "cd " + quotePath(e.Path) + " && " + script
	var cmd *exec.Cmd
	switch e.Kind {
	case KindSSH:
		t := "-T"
		if tty {
			t = "-t"
		}
		args := append(SSHOptions(), "-o", "LogLevel=ERROR", t, e.sshTarget())
		args = append(args, e.sshPortArgs()...)
		args = append(args, "--", full)
		cmd = exec.CommandContext(ctx, "ssh", args...)
	case KindDocker:
		flags := "-i"
		if tty {
			flags = "-it"
		}
		cmd = exec.CommandContext(ctx, "docker", "exec", flags, e.Docker, "sh", "-c", full)
	default:
		cmd = exec.CommandContext(ctx, "sh", "-c", full)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// CommandForwardAgent is Command with ssh agent forwarding, so the remote end
// can open its own ssh connection (server-to-server rsync).
// Agent forwarding is not honoured on multiplexed sessions, so this opens a
// dedicated connection.
func (e *Env) CommandForwardAgent(ctx context.Context, script string) *exec.Cmd {
	if e.Kind != KindSSH {
		return e.Command(ctx, script)
	}
	full := "cd " + quotePath(e.Path) + " && " + script
	args := append(configArgs(), "-A", "-o", "ControlPath=none", "-o", "ServerAliveInterval=30", "-T", e.sshTarget())
	args = append(args, e.sshPortArgs()...)
	args = append(args, "--", full)
	return exec.CommandContext(ctx, "ssh", args...)
}

// RunForwardAgent runs script with agent forwarding and returns stdout.
func (e *Env) RunForwardAgent(ctx context.Context, script string) ([]byte, error) {
	cmd := e.CommandForwardAgent(ctx, script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w\n%s", e.Name, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Run executes script, feeding stdin, and returns stdout. Errors include stderr.
func (e *Env) Run(ctx context.Context, script string, stdin []byte) ([]byte, error) {
	cmd := e.Command(ctx, script)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("%s: %w\n%s", e.Name, err, msg)
	}
	return out.Bytes(), nil
}

func (e *Env) sshTarget() string {
	host := e.SSH
	if m := hostPort.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return host
}

func (e *Env) sshPortArgs() []string {
	if m := hostPort.FindStringSubmatch(e.SSH); m != nil {
		return []string{"-p", m[2]}
	}
	return nil
}

var hostPort = regexp.MustCompile(`^(.+):(\d+)$`)

// SSHOptions are shared by every ssh invocation, including rsync's -e.
func SSHOptions() []string {
	dir := ControlDir()
	return append(configArgs(),
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(dir, "%C"),
		"-o", "ControlPersist=300",
		"-o", "ServerAliveInterval=30",
		"-o", "Compression=no",
		"-o", "Ciphers=aes128-gcm@openssh.com,chacha20-poly1305@openssh.com,aes256-gcm@openssh.com,aes128-ctr",
	)
}

// configArgs points ssh at $TELEPORT_SSH_CONFIG instead of ~/.ssh/config.
func configArgs() []string {
	if f := os.Getenv("TELEPORT_SSH_CONFIG"); f != "" {
		return []string{"-F", f}
	}
	return nil
}

// ControlDir holds ssh multiplexing sockets. Kept short: macOS caps socket
// paths at 104 bytes.
func ControlDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".teleport", "cm")
	os.MkdirAll(dir, 0o700)
	return dir
}

// SSH resolves the real user/host/port for this env (aliases like `wp:staging`
// cannot be used in rsync's host:path syntax).
func (e *Env) ResolveSSH() (SSHInfo, error) {
	e.sshOnce.Do(func() {
		args := append(append(configArgs(), "-G", e.sshTarget()), e.sshPortArgs()...)
		out, err := exec.Command("ssh", args...).Output()
		if err != nil {
			e.sshErr = fmt.Errorf("ssh -G %s: %w", e.SSH, err)
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			switch k {
			case "user":
				e.sshInfo.User = v
			case "hostname":
				e.sshInfo.Hostname = v
			case "port":
				e.sshInfo.Port = v
			case "identityfile":
				p := v
				if strings.HasPrefix(p, "~/") {
					home, _ := os.UserHomeDir()
					p = filepath.Join(home, p[2:])
				}
				if _, err := os.Stat(p); err == nil {
					e.sshInfo.Identities = append(e.sshInfo.Identities, p)
				}
			}
		}
	})
	return e.sshInfo, e.sshErr
}

// RsyncShell is the value for rsync -e when talking to this env.
func (e *Env) RsyncShell() (string, error) {
	info, err := e.ResolveSSH()
	if err != nil {
		return "", err
	}
	parts := []string{"ssh"}
	for _, o := range SSHOptions() {
		parts = append(parts, Quote(o))
	}
	if info.Port != "" && info.Port != "22" {
		parts = append(parts, "-p", info.Port)
	}
	for _, id := range info.Identities {
		parts = append(parts, "-i", Quote(id))
	}
	return strings.Join(parts, " "), nil
}

// RemoteSpec is `user@host:path` for rsync.
func (e *Env) RemoteSpec(path string) (string, error) {
	info, err := e.ResolveSSH()
	if err != nil {
		return "", err
	}
	return info.User + "@" + info.Hostname + ":" + path, nil
}

type mount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// HostDir maps an absolute path inside this env to a directory on this
// machine, or "" when the path is not reachable from here.
func (e *Env) HostDir(path string) string {
	switch e.Kind {
	case KindLocal:
		return path
	case KindSSH:
		return ""
	}
	if e.HostPath != "" {
		if rel, ok := under(path, e.Path); ok {
			return filepath.Join(e.HostPath, rel)
		}
	}
	e.mountOnce.Do(func() {
		out, err := exec.Command("docker", "inspect", "--format", "{{json .Mounts}}", e.Docker).Output()
		if err == nil {
			json.Unmarshal(out, &e.mounts)
		}
	})
	best := ""
	bestLen := -1
	for _, m := range e.mounts {
		if rel, ok := under(path, m.Destination); ok && len(m.Destination) > bestLen {
			if st, err := os.Stat(m.Source); err == nil && st.IsDir() {
				best = filepath.Join(m.Source, rel)
				bestLen = len(m.Destination)
			}
		}
	}
	return best
}

func under(path, root string) (string, bool) {
	root = strings.TrimRight(root, "/")
	if path == root {
		return "", true
	}
	if strings.HasPrefix(path, root+"/") {
		return path[len(root)+1:], true
	}
	return "", false
}

// Quote single-quotes s for POSIX shells.
func Quote(s string) string {
	if s != "" && safeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeShell = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// quotePath quotes a path but keeps a leading ~ expandable.
func quotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return `"$HOME"/` + Quote(rest)
	}
	return Quote(p)
}
