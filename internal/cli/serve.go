package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/serve"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// serveOption holds the arguments prefix + M-r gives tmag serve.
const serveOption = "@tmag-serve"

// serveWindow marks the tmux window tmag serve runs in.
const serveWindow = "@tmag_serve"

// serveFlags are tmag serve's settings, from its arguments.
type serveFlags struct {
	addr, name, owner, tailscale, access, team, aud, server string
	machines                                                []string
	open                                                    bool
}

func parseServe(args []string) (serveFlags, error) {
	var f serveFlags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&f.addr, "addr", "127.0.0.1:7777", "")
	fs.StringVar(&f.name, "name", "", "")
	fs.StringVar(&f.owner, "owner", "", "")
	fs.StringVar(&f.tailscale, "tailscale", "", "")
	fs.StringVar(&f.access, "access", "", "")
	fs.StringVar(&f.team, "access-team", "", "")
	fs.StringVar(&f.aud, "access-aud", "", "")
	fs.StringVar(&f.server, "server", "", "")
	fs.BoolVar(&f.open, "open", false, "")
	fs.Func("machine", "", func(s string) error {
		f.machines = append(f.machines, s)
		return nil
	})
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	return f, err
}

// config turns the flags into what serve serves; bin runs tmag here.
func (f serveFlags) config(bin, token string) (serve.Config, error) {
	name := f.name
	if name == "" {
		host, _ := os.Hostname()
		name, _, _ = strings.Cut(host, ".")
	}
	here, err := serve.ParseMachine(name + "=" + bin)
	if err != nil {
		return serve.Config{}, fmt.Errorf("--name: %w", err)
	}
	cfg := serve.Config{
		Machines: []serve.Machine{here}, Owner: f.owner, Token: token, Version: version.Version,
		Tailscale: splitList(f.tailscale),
		Access:    serve.Access{Hosts: splitList(f.access), Team: f.team, Audience: f.aud},
	}
	for _, s := range f.machines {
		m, err := serve.ParseMachine(s)
		if err != nil {
			return serve.Config{}, err
		}
		cfg.Machines = append(cfg.Machines, m)
	}
	return cfg, nil
}

// tunnelURL is the page's address through the first tunnel named, or "".
func (f serveFlags) tunnelURL() string {
	if hosts := append(splitList(f.tailscale), splitList(f.access)...); len(hosts) > 0 {
		return "https://" + hosts[0]
	}
	return ""
}

// cmdServe serves a page for the person's phone until stopped. With --open,
// as prefix + M-r runs it, it runs tmag serve in a window of the tmux server
// given, with the arguments @tmag-serve holds, or tells where it runs.
func (a *app) cmdServe(args []string) int {
	f, err := parseServe(args)
	if err != nil {
		return a.usageError("serve", err)
	}
	if f.open {
		return a.openServe(f.server)
	}
	bin, err := os.Executable()
	if err != nil {
		return a.fail("serve", err)
	}
	token, err := serveToken(a.log.Dir)
	if err != nil {
		return a.fail("serve", err)
	}
	cfg, err := f.config(bin, token)
	if err != nil {
		return a.usageError("serve", err)
	}
	logger := log.New(a.stdout, "", log.Ltime)
	cfg.Log = logger
	srv, err := serve.New(cfg)
	if err != nil {
		return a.usageError("serve", err)
	}
	ln, err := listenLoopback(f.addr)
	if err != nil {
		return a.fail("serve", err)
	}
	ctx, cancel := signalContext()
	defer cancel()

	var tmuxServer tmux.Server
	if f.server != "" {
		tmuxServer = tmux.ServerByName(f.server)
		if url := f.tunnelURL(); url != "" {
			_, _ = tmuxServer.Run(ctx, "set-option", "-g", notify.URLOption, url)
			defer func() {
				clear, done := context.WithTimeout(context.Background(), 2*time.Second)
				defer done()
				_, _ = tmuxServer.Run(clear, "set-option", "-gu", notify.URLOption)
			}()
		}
	}

	names := make([]string, len(cfg.Machines))
	for i, m := range cfg.Machines {
		names[i] = m.Name
	}
	fmt.Fprintf(a.stdout, "tmag serve %s: the agents on %s\n\n", version.Version, strings.Join(names, ", "))
	fmt.Fprintf(a.stdout, "  on this machine   http://%s/?token=%s\n", ln.Addr(), token)
	for _, h := range cfg.Tailscale {
		fmt.Fprintf(a.stdout, "  through Tailscale https://%s, for %s\n", h, cfg.Owner)
	}
	for _, h := range cfg.Access.Hosts {
		fmt.Fprintf(a.stdout, "  through Access    https://%s, for %s\n", h, cfg.Owner)
	}
	fmt.Fprintf(a.stdout, "\nPress Ctrl-C, or close this window, to stop. Actions taken:\n")

	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = hs.Shutdown(stop)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return a.fail("serve", err)
	}
	return ExitOK
}

// listenLoopback listens on addr, which must be on loopback: the way in from
// elsewhere is a tunnel, which proves who comes through it.
func listenLoopback(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("%s is not a loopback address: reach the page through Tailscale or Cloudflare instead", addr)
	}
	return ln, nil
}

// serveToken is the token that admits the person's browser on this machine,
// kept in the state directory so that a bookmark keeps working.
func serveToken(dir string) (string, error) {
	path := filepath.Join(dir, "serve.token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// openServe runs tmag serve in a new window of the tmux server, or tells
// where it already runs. Problems it can see before starting it, such as an
// address in use, are shown as a tmux message instead.
func (a *app) openServe(server string) int {
	if server == "" {
		return a.usageError("serve", errors.New("--open needs --server"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := tmux.ServerByName(server)
	say := func(msg string) int {
		_, _ = srv.Run(ctx, "display-message", "-d", "8000", msg)
		return ExitOK
	}
	if out, err := srv.Run(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{"+serveWindow+"}\t#{session_name}:#{window_index}"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f := strings.Split(line, "\t"); len(f) == 3 && f[1] != "" {
				url := srv.GlobalOption(ctx, notify.URLOption)
				if url == "" {
					url = "this machine only"
				}
				return say("tmag serve runs in window " + f[2] + ": " + url)
			}
		}
	}
	args, err := splitArgs(srv.GlobalOption(ctx, serveOption))
	if err != nil {
		return say("tmag serve: " + serveOption + ": " + err.Error())
	}
	f, err := parseServe(args)
	if err == nil && (f.open || f.server != "") {
		err = errors.New(serveOption + " takes neither --open nor --server, which the key sets itself")
	}
	var cfg serve.Config
	if err == nil {
		cfg, err = f.config("tmag", "check")
	}
	if err == nil {
		_, err = serve.New(cfg)
	}
	if err == nil {
		var ln net.Listener
		if ln, err = listenLoopback(f.addr); err == nil {
			ln.Close()
		}
	}
	if err != nil {
		return say("tmag serve: " + err.Error())
	}
	bin, err := os.Executable()
	if err != nil {
		return say("tmag serve: " + err.Error())
	}
	argv := append([]string{bin, "serve", "--server", server}, args...)
	quoted := make([]string, len(argv))
	for i, s := range argv {
		quoted[i] = shellQuote(s)
	}
	out, err := srv.Run(ctx, "new-window", "-d", "-P", "-F", "#{window_id}\t#{session_name}:#{window_index}", "-n", "tmag serve", strings.Join(quoted, " "))
	if err != nil {
		return say("tmag serve: " + err.Error())
	}
	id, where, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	_, _ = srv.Run(ctx, "set-option", "-w", "-t", id, serveWindow, "1")
	url := f.tunnelURL()
	if url == "" {
		url = "this machine only; see the window for its address"
	}
	return say("tmag serve started in window " + where + ": " + url)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitArgs splits an option's value into arguments the way sh would, with
// single and double quotes and backslashes, and nothing expanded.
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated '")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inArg = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New(`unterminated "`)
			}
			inArg = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inArg = true
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// checkServe reports whether tmag serve runs behind a tunnel.
func checkServe() check {
	for _, srv := range tmux.Servers() {
		if url := srv.GlobalOption(context.Background(), notify.URLOption); url != "" {
			return check{"serve", "ok", "tmag serve runs at " + url}
		}
	}
	return check{"serve", "skip", "tmag serve does not run behind a tunnel (prefix + M-r opens it)"}
}
