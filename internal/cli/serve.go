package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/serve"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

const serveOption = "@tmag-serve"
const serveWindow = "@tmag_serve"
const serveAddressOption = "@tmag_api_addr"

type serveFlags struct {
	addr, server   string
	hosts, origins []string
	open           bool
}

func parseServe(args []string) (serveFlags, error) {
	f := serveFlags{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&f.addr, "addr", "127.0.0.1:7777", "")
	fs.StringVar(&f.server, "server", "", "")
	fs.BoolVar(&f.open, "open", false, "")
	fs.Func("host", "", func(s string) error { f.hosts = append(f.hosts, s); return nil })
	fs.Func("origin", "", func(s string) error { f.origins = append(f.origins, s); return nil })
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	return f, err
}
func (a *app) serveConfig(f serveFlags) serve.Config {
	cfg := serve.Config{Auth: auth.Store{Dir: auth.DefaultDir()}, Hosts: f.hosts, Origins: f.origins, Version: version.Version, Log: log.New(a.stderr, "", log.Ltime)}
	cfg.Run = func(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
		if in == nil {
			in = strings.NewReader("")
		}
		local := &app{ctx: ctx, stdin: in, stdout: out, stderr: stderr, log: a.log}
		return local.run(args)
	}
	return cfg
}

func (a *app) cmdServe(args []string) int {
	f, err := parseServe(args)
	if err != nil {
		return a.usageError("serve", err)
	}
	if f.open {
		return a.openServe(f.server)
	}
	handler, err := serve.New(a.serveConfig(f))
	if err != nil {
		return a.usageError("serve", err)
	}
	ln, err := listenLoopback(f.addr)
	if err != nil {
		return a.fail("serve", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	if f.server != "" {
		srv := tmux.ServerByName(f.server)
		_, _ = srv.Run(ctx, "set-option", "-g", serveAddressOption, "http://"+ln.Addr().String())
		defer func() {
			quick, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			_, _ = srv.Run(quick, "set-option", "-gu", serveAddressOption)
		}()
	}
	fmt.Fprintf(a.stdout, "tmag serve %s (%s)\nhttp://%s/v1/info\nUse tmag auth issue or tmag auth pair to authorize a client.\nPress Ctrl-C, or close this window, to stop.\n", version.Version, serve.Schema, ln.Addr())
	hs := http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		stop, finish := context.WithTimeout(context.Background(), 125*time.Second)
		defer finish()
		_ = hs.Shutdown(stop)
	}()
	err = hs.Serve(ln)
	cancel()
	<-done
	if !errors.Is(err, http.ErrServerClosed) {
		return a.fail("serve", err)
	}
	return ExitOK
}
func listenLoopback(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return nil, errors.New("serve listens on loopback; use an authenticated, encrypted tunnel for remote access")
	}
	return ln, nil
}
func (a *app) openServe(server string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := tmux.DefaultServer()
	if server != "" {
		srv = tmux.ServerByName(server)
	}
	out, err := srv.Run(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{"+serveWindow+"}")
	if err != nil {
		return a.fail("serve", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 2 && f[1] != "" {
			fmt.Fprintf(a.stdout, "tmag serve runs in window %s\n", f[0])
			return ExitOK
		}
	}
	args, err := splitArgs(srv.GlobalOption(ctx, serveOption))
	if err != nil {
		return a.usageError("serve", err)
	}
	f, err := parseServe(args)
	if err != nil {
		return a.usageError("serve", err)
	}
	if f.open || f.server != "" {
		return a.usageError("serve", errors.New("@tmag-serve takes neither --open nor --server"))
	}
	if _, err := serve.New(a.serveConfig(f)); err != nil {
		return a.usageError("serve", err)
	}
	listener, err := listenLoopback(f.addr)
	if err != nil {
		return a.fail("serve", err)
	}
	listener.Close()
	bin, err := os.Executable()
	if err != nil {
		return a.fail("serve", err)
	}
	command := shellQuote(bin) + " serve --server " + shellQuote(srv.Socket)
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	// Pass scratch/instance directories explicitly: a running tmux server can
	// predate the environment from which the owner opens this endpoint.
	for _, key := range []string{"TMAG_STATE_DIR", "TMAG_AUTH_DIR"} {
		if value := os.Getenv(key); value != "" {
			command = key + "=" + shellQuote(value) + " " + command
		}
	}
	window, err := srv.Run(ctx, "new-window", "-d", "-P", "-F", "#{window_id}", "-n", "tmag-api", command)
	if err != nil {
		return a.fail("serve", err)
	}
	id := strings.TrimSpace(string(window))
	_, err = srv.Run(ctx, "set-option", "-w", "-t", id, serveWindow, "1")
	if err != nil {
		return a.fail("serve", err)
	}
	fmt.Fprintf(a.stdout, "tmag serve started in window %s\n", id)
	return ExitOK
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
		if addr := srv.GlobalOption(context.Background(), serveAddressOption); addr != "" {
			return check{"serve", "ok", "local HTTP API at " + addr}
		}
	}
	return check{"serve", "skip", "optional local HTTP API is stopped (prefix + M-r opens it)"}
}
