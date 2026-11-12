// Package tmux wraps the tmux CLI for one server socket at a time.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
)

// callTimeout bounds a single tmux invocation; callers add overall deadlines.
const callTimeout = 2 * time.Second

// Server addresses one tmux server by its canonical socket path.
type Server struct {
	Socket string
}

// Pane is one row of list-panes with the tmag user options attached.
type Pane struct {
	Server  string
	ID      string
	PID     int
	Command string
	Path    string
	Session string
	Window  int
	Index   int
	Title   string
	// TTY is the pane's terminal device.
	TTY string
	// Watched is true when an attached session shows this pane as its active pane.
	Watched bool
	Options map[string]string
}

// Target is the human session:window.pane address.
func (p Pane) Target() string {
	return fmt.Sprintf("%s:%d.%d", p.Session, p.Window, p.Index)
}

// OptionPrefix scopes every pane option owned by tmag.
const OptionPrefix = "@tmag_"

// OptionSeen is the plugin's switch for learning seen turns from the
// terminal: `on` (the default when unset) or `off`.
const OptionSeen = "@tmag-seen"

// OptionKeys lists every pane option tmag reads or writes, without the
// prefix: the attachment (agent, session, pid, start, evidence) and the
// display projection (state, kind, since, cmd).
var OptionKeys = []string{"agent", "session", "pid", "start", "evidence", "state", "kind", "since", "cmd"}

// sep separates the fields of a pane line. It must be a byte every tmux
// prints as is: tmux 3.4 writes other control characters in -F output as
// octal escapes without escaping backslashes, which no parser could undo,
// but leaves tabs alone. clean() removes it from every free-text field.
const sep = "\t"

// clean strips C0 controls and DEL from a free-text format value, so text set
// by programs in a pane (paths, titles, names) can never inject the field
// separator or a newline. The regex is written with literal bytes because a
// POSIX class such as [[:cntrl:]] would be split at its colons by tmux.
func clean(name string) string {
	return "#{s/[\x01-\x1f\x7f]//:" + name + "}"
}

var paneFormat = func() string {
	fields := []string{
		"#{pane_id}", "#{pane_pid}", clean("pane_current_command"), clean("pane_current_path"),
		clean("session_name"), "#{window_index}", "#{pane_index}", "#{pane_tty}",
		"#{&&:#{session_attached},#{&&:#{window_active},#{pane_active}}}",
	}
	for _, k := range OptionKeys {
		fields = append(fields, clean(OptionPrefix+k))
	}
	fields = append(fields, clean("pane_title"))
	return strings.Join(fields, sep)
}()

var paneIDRE = regexp.MustCompile(`^%\d+$`)

// Run executes a tmux command against the server.
func (s Server) Run(ctx context.Context, args ...string) ([]byte, error) {
	return s.RunInput(ctx, nil, args...)
}

// RunInput executes a tmux command with stdin.
func (s Server) RunInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	// -u: without a UTF-8 locale (hooks often run without one) tmux would
	// replace the field separator and any non-ASCII text with '_'.
	cmd, cleanup := helper.Command(ctx, "tmux", append([]string{"-u", "-S", s.Socket}, args...)...)
	defer cleanup()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return out, err
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// Panes lists every pane on the server.
func (s Server) Panes(ctx context.Context) ([]Pane, error) {
	out, err := s.Run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if p, ok := s.parsePane(line); ok {
			panes = append(panes, p)
		}
	}
	return panes, nil
}

// PaneID resolves a target-pane the way tmux's own commands do (a session,
// session:window.pane, a window name, {last}, {marked}, ...) and returns the
// pane's id. display-message alone would name the current pane for a target
// it cannot find, so capture-pane resolves it first, strictly: with -P it
// captures only an unfinished escape sequence, usually nothing, and no line
// of the pane.
func (s Server) PaneID(ctx context.Context, target string) (string, error) {
	out, err := s.Run(ctx, "capture-pane", "-p", "-P", "-t", target, ";", "display-message", "-p", "-t", target, "#{pane_id}")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	id := lines[len(lines)-1]
	if !paneIDRE.MatchString(id) {
		return "", fmt.Errorf("target %s: unexpected tmux output", target)
	}
	return id, nil
}

// Pane reads one pane afresh.
func (s Server) Pane(ctx context.Context, id string) (Pane, error) {
	out, err := s.Run(ctx, "display-message", "-p", "-t", id, paneFormat)
	if err != nil {
		return Pane{}, err
	}
	p, ok := s.parsePane(strings.TrimRight(string(out), "\n"))
	if !ok || p.ID != id {
		return Pane{}, fmt.Errorf("pane %s: unexpected tmux output", id)
	}
	return p, nil
}

func (s Server) parsePane(line string) (Pane, bool) {
	const fixed = 9
	f := strings.Split(line, sep)
	if len(f) != fixed+len(OptionKeys)+1 || !paneIDRE.MatchString(f[0]) {
		return Pane{}, false
	}
	pid, _ := strconv.Atoi(f[1])
	win, _ := strconv.Atoi(f[5])
	idx, _ := strconv.Atoi(f[6])
	p := Pane{
		Server: s.Socket, ID: f[0], PID: pid, Command: f[2], Path: f[3],
		Session: f[4], Window: win, Index: idx, TTY: f[7], Watched: f[8] == "1",
		Title: f[len(f)-1], Options: map[string]string{},
	}
	for i, k := range OptionKeys {
		if v := f[fixed+i]; v != "" {
			p.Options[k] = v
		}
	}
	return p, true
}

// SetPaneOptions sets and unsets tmag pane options in a single tmux invocation.
func (s Server) SetPaneOptions(ctx context.Context, pane string, set map[string]string, unset []string) error {
	var args []string
	add := func(cmd ...string) {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, cmd...)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("set-option", "-p", "-t", pane, OptionPrefix+k, set[k])
	}
	for _, k := range unset {
		add("set-option", "-p", "-u", "-t", pane, OptionPrefix+k)
	}
	if len(args) == 0 {
		return nil
	}
	_, err := s.Run(ctx, args...)
	return err
}

// ClearPaneOptions removes every tmag option from the pane.
func (s Server) ClearPaneOptions(ctx context.Context, pane string) error {
	return s.SetPaneOptions(ctx, pane, nil, OptionKeys)
}

// Capture returns the last n lines of the pane, joining wrapped lines.
func (s Server) Capture(ctx context.Context, pane string, lines int) (string, error) {
	out, err := s.Run(ctx, "capture-pane", "-p", "-J", "-t", pane, "-S", strconv.Itoa(-lines))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n") + "\n", nil
}

// Submit types text into the pane as a bracketed paste and presses Enter.
// Control characters are removed first (see SanitizePrompt), so the text can
// neither end the paste early nor press keys of its own.
func (s Server) Submit(ctx context.Context, pane, text string) error {
	buf := fmt.Sprintf("tmag-%d-%d", os.Getpid(), time.Now().UnixNano())
	if _, err := s.RunInput(ctx, []byte(SanitizePrompt(text)), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	_, err := s.Run(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane, ";", "send-keys", "-t", pane, "Enter")
	return err
}

// Interrupt presses Escape in the pane, the key every agent tmag knows stops
// a running turn with.
func (s Server) Interrupt(ctx context.Context, pane string) error {
	_, err := s.Run(ctx, "send-keys", "-t", pane, "Escape")
	return err
}

// SanitizePrompt normalises line endings to \n and drops every other C0 and C1
// control character and DEL, keeping tabs.
func SanitizePrompt(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		}
		return r
	}, text)
}

// GlobalOption returns a global option's value, or "" when unset or unreadable.
// Call GlobalOptionContext when an unavailable server must remain distinguishable.
func (s Server) GlobalOption(ctx context.Context, name string) string {
	value, _ := s.GlobalOptionContext(ctx, name)
	return value
}

// GlobalOptionContext returns an unset option as "", nil while preserving
// command, cancellation and decoding errors. It performs no value expansion.
func (s Server) GlobalOptionContext(ctx context.Context, name string) (string, error) {
	values, err := s.GlobalOptionsContext(ctx, name)
	if err != nil {
		return "", err
	}
	return values[name], nil
}

// GlobalOptionsContext returns several global options in one tmux
// invocation, sparing a hook a process per option. An unset option is "".
func (s Server) GlobalOptionsContext(ctx context.Context, names ...string) (map[string]string, error) {
	values := make(map[string]string, len(names))
	if len(names) == 0 {
		return values, nil
	}
	// Read tmux's quoted representation so literal backslashes and control
	// characters remain distinguishable. tmux 3.4 additionally escapes dollar
	// variables when printing; probe that layer in the same command invocation.
	const probe = "$TMAG_OPTION_PRINT"
	var args []string
	for _, name := range names {
		args = append(args, "show-option", "-gq", name, ";")
	}
	args = append(args, "display-message", "-p", probe)
	out, err := s.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	escaped := false
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == `\`+probe {
		escaped = true
		lines = lines[:len(lines)-1]
	}
	for _, name := range names {
		for _, line := range lines {
			value, ok := strings.CutPrefix(line, name+" ")
			if !ok {
				continue
			}
			if escaped {
				value = unescapePrintedDollars(value)
			}
			if value, err = unquoteOption(value); err != nil {
				return nil, err
			}
			values[name] = strings.TrimSpace(value)
			break
		}
	}
	return values, nil
}

// unescapePrintedDollars reverses just the extra printing layer of tmux 3.4.
// An intentional backslash before a variable remains in the quoted layer.
func unescapePrintedDollars(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+2 < len(s) && s[i+1] == '$' {
			c := s[i+2]
			if c == '_' || c == '{' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// unquoteOption reads the quoted C-style representation printed by tmux's
// args_escape. It performs no shell, environment or format expansion.
func unquoteOption(s string) (string, error) {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", fmt.Errorf("unfinished tmux option escape")
		}
		if s[i] >= '0' && s[i] <= '7' {
			end := i + 1
			for end < len(s) && end < i+3 && s[end] >= '0' && s[end] <= '7' {
				end++
			}
			n, err := strconv.ParseUint(s[i:end], 8, 8)
			if err != nil {
				return "", err
			}
			out.WriteByte(byte(n))
			i = end - 1
			continue
		}
		if n := strings.IndexByte("nrbavtfs", s[i]); n >= 0 {
			out.WriteByte("\n\r\b\a\v\t\f "[n])
		} else {
			out.WriteByte(s[i])
		}
	}
	return out.String(), nil
}

// Bell rings the pane's terminal bell, which tmux turns into the window's
// alert flag. A single BEL byte is written to the pane's terminal without
// waiting: when the terminal cannot take it at once (output stopped, buffer
// full), the bell is dropped rather than holding up the caller.
func (p Pane) Bell() error {
	if !strings.HasPrefix(p.TTY, "/dev/") {
		return fmt.Errorf("pane %s has no terminal", p.ID)
	}
	// Raw system calls: an *os.File would park on EAGAIN until the terminal
	// drains, which is exactly the wait to avoid.
	fd, err := syscall.Open(p.TTY, syscall.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if _, err := syscall.Write(fd, []byte{'\a'}); err != nil {
		return fmt.Errorf("bell %s: %w", p.TTY, err)
	}
	return nil
}

// LastInput is when a client of the server last had input from the person:
// keys or the mouse. It is false when no client is attached.
func (s Server) LastInput(ctx context.Context) (time.Time, bool) {
	out, err := s.Run(ctx, "list-clients", "-F", "#{client_activity}")
	if err != nil {
		return time.Time{}, false
	}
	var last int64
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil && n > last {
			last = n
		}
	}
	if last == 0 {
		return time.Time{}, false
	}
	return time.Unix(last, 0), true
}

// Version reports the server's tmux version string, e.g. "3.3a".
func (s Server) Version(ctx context.Context) (string, error) {
	out, err := s.Run(ctx, "display-message", "-p", "#{version}")
	return strings.TrimSpace(string(out)), err
}

// canonical resolves symlinks in a socket's directory, the way tmux itself
// does, so one server is always spelled the same (macOS: /tmp -> /private/tmp).
func canonical(socket string) string {
	dir, name := filepath.Split(socket)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(real, name)
	}
	return filepath.Clean(socket)
}

// SocketDir is the directory where tmux places sockets for the current user.
func SocketDir() string {
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	dir := filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()))
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// SocketFromEnv extracts the socket path from a $TMUX value ("socket,pid,session").
func SocketFromEnv(tmuxEnv string) string {
	if i := strings.IndexByte(tmuxEnv, ','); i > 0 {
		tmuxEnv = tmuxEnv[:i]
	}
	return canonical(tmuxEnv)
}

// ServerByName resolves a socket name (as for tmux -L) or a path to a Server.
func ServerByName(name string) Server {
	if strings.ContainsRune(name, '/') {
		return Server{Socket: canonical(name)}
	}
	return Server{Socket: filepath.Join(SocketDir(), name)}
}

// DefaultServer is the server the caller is attached to, or the default socket.
func DefaultServer() Server {
	if env := os.Getenv("TMUX"); env != "" {
		return Server{Socket: SocketFromEnv(env)}
	}
	return ServerByName("default")
}

// Servers lists every tmux socket of the current user, the caller's server first.
// Sockets are returned whether or not a server is listening on them.
func Servers() []Server {
	servers, _ := ServersContext(context.Background())
	return servers
}

// ServersContext enumerates sockets in bounded batches within the caller budget.
func ServersContext(ctx context.Context) ([]Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Server
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, Server{Socket: p})
	}
	if env := os.Getenv("TMUX"); env != "" {
		add(SocketFromEnv(env))
	}
	dir := SocketDir()
	err := contextio.Entries(ctx, dir, func(e os.DirEntry) error {
		if e.Type()&os.ModeSocket != 0 {
			add(filepath.Join(dir, e.Name()))
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

// VersionAtLeast compares a tmux version string such as "3.3a" or "next-3.6".
func VersionAtLeast(v string, major, minor int) bool {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return strings.HasPrefix(v, "next") || v == "master"
	}
	ma, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return ma > major || (ma == major && mi >= minor)
}
