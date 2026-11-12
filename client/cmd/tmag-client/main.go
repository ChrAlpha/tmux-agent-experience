// tmag-client is the remote companion: the agents in tmux on the machines it
// is given, from a phone.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/gateway"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/httpclient"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/version"
)

const usage = `tmag-client is the remote companion: a page for one person's phone, over
the tmag/v2 contract of each machine it is given.

  tmag-client serve [flags]     run the gateway, in the foreground of a tmux window
  tmag-client auth pair LABEL [--ttl D] [--json]
                                create a single-use invitation and print its code
  tmag-client auth list [--json]
                                list paired devices
  tmag-client auth revoke ID    revoke one device
  tmag-client --version         print the client's version and its contracts

Serve flags:

  --addr ADDR             loopback HTTP listen address (default 127.0.0.1:7778)
  --machine NAME=CMD      a command that runs tmag, such as box=ssh box tmag; repeatable
  --endpoint NAME=URL     a tmag/http/v2 endpoint; repeatable
  --credential NAME=FILE  an endpoint's credential; repeatable
  --owner IDENTITY        who a tunnel must prove: a Tailscale login or an Access email
  --tailscale HOSTS       comma-separated Tailscale Serve host names
  --access HOSTS          comma-separated Cloudflare Access host names
  --access-team TEAM      the Cloudflare Access team that signs its tokens
  --access-aud AUD        the Cloudflare Access audience tag of this application
  --state-dir DIR         the gateway's persistent authorization directory
  --push KINDS            alert kinds to push, comma-separated (default blocked,finished;
                          empty turns push off)

Auth flags:

  --ttl D                 credential lifetime (default: until revoked)
  --json                  print the document instead of a line
  --state-dir DIR         the gateway's persistent authorization directory

Exit codes: 0 ok, 1 error, 2 usage.
`

// errUsage marks a command line the client cannot read, which exits 2.
var errUsage = errors.New("usage")

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "http":
		os.Exit(worker(args[1:]))
	case len(args) > 0 && (args[0] == "--version" || args[0] == "-V"):
		fmt.Printf("tmag-client %s (%s, %s)\n", version.Version, version.State, version.HTTP)
		return
	case len(args) > 0 && args[0] == "serve":
		if err := serve(args[1:]); err != nil {
			fail(err)
		}
	case len(args) > 0 && args[0] == "auth":
		if err := authCommand(args[1:]); err != nil {
			fail(err)
		}
	case len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h"):
		fmt.Print(usage)
	default:
		if len(args) > 0 {
			fmt.Fprintf(os.Stderr, "tmag-client: unknown command %q\n\n", args[0])
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// worker runs one tmag verb against an HTTP endpoint, as the gateway's HTTP
// backends invoke it: an internal shape between serve and its own process,
// never a command people call.
func worker(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "HTTP worker needs an endpoint, credential file and operation")
		return 2
	}
	b, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read endpoint credential")
		return 1
	}
	token := strings.TrimSpace(string(b))
	if strings.HasPrefix(token, "{") {
		var issued struct{ Credential struct{ Token string } }
		if json.Unmarshal(b, &issued) != nil {
			return 1
		}
		token = issued.Credential.Token
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := httpclient.Run(ctx, args[0], token, args[2:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	return code
}

// parseAll reads flags wherever they appear among positional arguments, as
// tmag's own commands do.
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, arg := range args {
		if arg == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// authCommand manages the devices paired with this gateway. It works on the
// state directory whether or not serve runs.
func authCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected pair, list or revoke", errUsage)
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("tmag-client auth "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	jsonOut := fs.Bool("json", false, "print the document")
	ttl := fs.Duration("ttl", 0, "credential lifetime")
	dir := fs.String("state-dir", defaultState(), "gateway's persistent authorization directory")
	pos, err := parseAll(fs, rest)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *ttl < 0 {
		return fmt.Errorf("%w: credential lifetime must be positive", errUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	store := auth.Store{Dir: *dir}
	switch verb {
	case "pair":
		if len(pos) != 1 {
			return fmt.Errorf("%w: auth pair needs one label", errUsage)
		}
		p, err := store.Pair(ctx, pos[0], *ttl)
		if err != nil {
			return err
		}
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"pairing": p})
			return nil
		}
		fmt.Fprintln(os.Stdout, p.Code)
		fmt.Fprintf(os.Stderr, "Pair once before %s. A successful pairing issues its own credential.\n", p.Expires.Format(time.RFC3339))
	case "list":
		if len(pos) != 0 {
			return fmt.Errorf("%w: auth list takes no arguments", errUsage)
		}
		instance, list, err := store.List(ctx)
		if err != nil {
			return err
		}
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"instance": instance, "credentials": list})
			return nil
		}
		for _, c := range list {
			expiry := "until revoked"
			if c.Expires != nil {
				expiry = c.Expires.Format(time.RFC3339)
			}
			push := "no push"
			if c.Push {
				push = "push"
			}
			fmt.Fprintf(os.Stdout, "%s\t%q\t%s\t%s\t%s\n", c.ID, c.Label, c.Created.Format(time.RFC3339), expiry, push)
		}
	case "revoke":
		if len(pos) != 1 {
			return fmt.Errorf("%w: auth revoke needs one id", errUsage)
		}
		if err := store.Revoke(ctx, pos[0]); err != nil {
			return err
		}
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"revoked": pos[0]})
			return nil
		}
		fmt.Fprintln(os.Stdout, "Authorization revoked.")
	default:
		return fmt.Errorf("%w: expected pair, list or revoke", errUsage)
	}
	return nil
}

func serve(args []string) error {
	var machines, endpoints, credentials values
	fs := flag.NewFlagSet("tmag-client serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7778", "loopback HTTP listen address")
	owner := fs.String("owner", "", "owner identity proven by a tunnel")
	tailscale := fs.String("tailscale", "", "comma-separated Tailscale host names")
	access := fs.String("access", "", "comma-separated Cloudflare Access host names")
	team := fs.String("access-team", "", "Cloudflare Access team")
	audience := fs.String("access-aud", "", "Cloudflare Access audience")
	dir := fs.String("state-dir", defaultState(), "gateway's persistent authorization directory")
	pushKinds := fs.String("push", "blocked,finished", "alert kinds to push, comma-separated")
	fs.Var(&endpoints, "endpoint", "NAME=URL for a tmag/http/v2 endpoint; repeatable")
	fs.Var(&credentials, "credential", "NAME=FILE holding an endpoint credential; repeatable")
	fs.Var(&machines, "machine", "NAME=COMMAND that runs tmag; repeatable")
	// A usage error gets one line from main, not the flag package's own.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, "usage: tmag-client serve [flags]\n\n")
			fs.SetOutput(os.Stdout)
			fs.PrintDefaults()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: unexpected positional arguments", errUsage)
	}
	if len(machines)+len(endpoints) == 0 {
		machines = values{"local=tmag"}
	}
	cfg := gateway.Config{Owner: *owner, Tailscale: split(*tailscale), Access: gateway.Access{Hosts: split(*access), Team: *team, Audience: *audience}, StateDir: *dir, Push: splitKinds(*pushKinds), Version: version.Version, Log: log.New(os.Stderr, "", log.Ltime)}
	for _, m := range machines {
		parsed, err := gateway.ParseMachine(m)
		if err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		cfg.Machines = append(cfg.Machines, parsed)
	}

	files := map[string]string{}
	for _, spec := range credentials {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("%w: credential must be NAME=FILE", errUsage)
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("%w: duplicate credential mapping", errUsage)
		}
		files[name] = path
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for _, spec := range endpoints {
		name, endpoint, ok := strings.Cut(spec, "=")
		if !ok {
			return fmt.Errorf("%w: endpoint must be NAME=URL", errUsage)
		}
		if _, err := httpclient.Endpoint(endpoint); err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		m, err := gateway.ParseMachine(name + "=tmag")
		if err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		file := files[name]
		if file == "" {
			return fmt.Errorf("%w: endpoint needs a --credential mapping", errUsage)
		}
		delete(files, name)
		m.Argv = []string{self, "http", endpoint, file}
		cfg.Machines = append(cfg.Machines, m)
	}
	if len(files) > 0 {
		return fmt.Errorf("%w: credential mapping has no endpoint", errUsage)
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	handler, err := gateway.New(cfg)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	defer handler.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	inv, err := auth.Store{Dir: *dir}.Pair(ctx, "local browser", 0)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("%w: the client gateway listens on loopback", errUsage)
	}
	fmt.Fprintf(os.Stdout, "tmag-client at http://%s/#pair=%s\n", ln.Addr(), inv.Code)
	server := http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 125*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	err = server.Serve(ln)
	handler.Close()
	stop()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tmag-client:", err)
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

func defaultState() string {
	if p := os.Getenv("TMAG_CLIENT_STATE_DIR"); p != "" {
		return p
	}
	if p := os.Getenv("XDG_STATE_HOME"); p != "" {
		return filepath.Join(p, "tmag-client")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tmag-client")
}

func splitKinds(s string) []string {
	out := []string{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func split(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, x := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(x))
	}
	return out
}
