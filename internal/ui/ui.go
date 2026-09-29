// Package ui prints human output to stderr (stdout is reserved for --json).
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type UI struct {
	Quiet   bool
	Verbose bool
	w       io.Writer
	tty     bool
	mu      sync.Mutex
	color   bool
}

func New(quiet, verbose bool) *UI {
	tty := isTTY(os.Stderr)
	return &UI{Quiet: quiet, Verbose: verbose, w: os.Stderr, tty: tty, color: tty && os.Getenv("NO_COLOR") == ""}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) Bold(s string) string   { return u.paint("1", s) }
func (u *UI) Dim(s string) string    { return u.paint("2", s) }
func (u *UI) Red(s string) string    { return u.paint("31", s) }
func (u *UI) Green(s string) string  { return u.paint("32", s) }
func (u *UI) Yellow(s string) string { return u.paint("33", s) }
func (u *UI) Cyan(s string) string   { return u.paint("36", s) }

func (u *UI) print(s string) {
	if u.Quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tty {
		fmt.Fprint(u.w, "\r\x1b[K")
	}
	fmt.Fprintln(u.w, s)
}

func (u *UI) Println(format string, args ...any) { u.print(fmt.Sprintf(format, args...)) }
func (u *UI) Step(format string, args ...any) {
	u.print(u.Cyan("> ") + fmt.Sprintf(format, args...))
}
func (u *UI) Done(format string, args ...any) {
	u.print(u.Green("ok ") + fmt.Sprintf(format, args...))
}
func (u *UI) Warn(format string, args ...any) {
	u.print(u.Yellow("warning: ") + fmt.Sprintf(format, args...))
}
func (u *UI) Debug(format string, args ...any) {
	if u.Verbose {
		u.print(u.Dim(fmt.Sprintf(format, args...)))
	}
}

// Progress overwrites the current terminal line (empty clears it). No-op
// without a terminal.
func (u *UI) Progress(line string) {
	if !u.tty || u.Quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.w, "\r\x1b[K")
	if line != "" {
		fmt.Fprint(u.w, u.Dim("  "+line))
	}
}

// Confirm asks the user to type expected. It fails without a terminal.
func (u *UI) Confirm(prompt, expected string) error {
	if !isTTY(os.Stdin) {
		return fmt.Errorf("%s: no terminal to confirm on; pass --confirm=%s or TELEPORT_CONFIRM=%s", prompt, expected, expected)
	}
	u.mu.Lock()
	fmt.Fprintf(u.w, "%s\nType %s to continue: ", prompt, u.Bold(expected))
	u.mu.Unlock()
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != expected {
		return fmt.Errorf("aborted")
	}
	return nil
}

// YesNo asks a y/N question; non-interactive sessions get "no".
func (u *UI) YesNo(prompt string) bool {
	if !isTTY(os.Stdin) {
		return false
	}
	u.mu.Lock()
	fmt.Fprintf(u.w, "%s [y/N] ", prompt)
	u.mu.Unlock()
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// Meter shows live byte progress on a single terminal line.
type Meter struct {
	u     *UI
	label string
	total int64
	done  atomic.Int64
	extra atomic.Value
	start time.Time
	stop  chan struct{}
	wg    sync.WaitGroup
}

func (u *UI) Meter(label string, total int64) *Meter {
	m := &Meter{u: u, label: label, total: total, start: time.Now(), stop: make(chan struct{})}
	m.extra.Store("")
	if u.tty && !u.Quiet {
		m.wg.Add(1)
		go m.loop()
	}
	return m
}

func (m *Meter) Add(n int64)            { m.done.Add(n) }
func (m *Meter) SetExtra(s string)      { m.extra.Store(s) }
func (m *Meter) Bytes() int64           { return m.done.Load() }
func (m *Meter) Elapsed() time.Duration { return time.Since(m.start) }

func (m *Meter) Write(p []byte) (int, error) {
	m.done.Add(int64(len(p)))
	return len(p), nil
}

func (m *Meter) loop() {
	defer m.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.render()
		}
	}
}

func (m *Meter) render() {
	done := m.done.Load()
	secs := time.Since(m.start).Seconds()
	rate := float64(done) / max(secs, 0.001)
	line := fmt.Sprintf("  %s  %s", m.label, Bytes(done))
	if m.total > 0 {
		line += " / ~" + Bytes(m.total)
	}
	line += fmt.Sprintf("  %s/s  %s", Bytes(int64(rate)), m.extra.Load().(string))
	m.u.mu.Lock()
	fmt.Fprint(m.u.w, "\r\x1b[K"+m.u.Dim(line))
	m.u.mu.Unlock()
}

func (m *Meter) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.wg.Wait()
	if m.u.tty && !m.u.Quiet {
		m.u.mu.Lock()
		fmt.Fprint(m.u.w, "\r\x1b[K")
		m.u.mu.Unlock()
	}
}

// Bytes formats a byte count.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Duration formats a duration compactly.
func Duration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
