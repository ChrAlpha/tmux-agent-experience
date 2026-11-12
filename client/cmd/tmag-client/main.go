// tmag-client is the remote companion: one person's page for the coding
// agents in tmux on the machines it follows, served by a gateway that runs in
// a tmux window.
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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/devices"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/gateway"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/qr"
	"github.com/ChrAlpha/tmux-agent-experience/client/internal/version"
)

const usage = `tmag-client is the remote companion: a page for the coding agents in tmux on
the machines it follows, served from a tmux window of this machine.

Usage:
  tmag-client serve [--public URL]... [--machine NAME=COMMAND]... [--endpoint NAME=URL --credential NAME=FILE]...
                    [--addr ADDR] [--push KINDS] [--push-service HOST]... [--state-dir DIR]
                                     run the gateway until stopped
  tmag-client auth pair [LABEL] [--ttl D] [--json] [--state-dir DIR]
                                     invite one device: a code, and a QR code of its address
  tmag-client auth list [--json] [--state-dir DIR]
                                     list paired devices
  tmag-client auth revoke ID [--state-dir DIR]
                                     unpair a device
  tmag-client --version

Serve:
  --public URL            an https origin a tunnel reaches the gateway at, such as
                          https://box.example.ts.net; repeatable; push needs one
  --machine NAME=COMMAND  a machine and the command that runs tmag there, such as
                          box='ssh box tmag'; NAME alone is this machine's tmag;
                          repeatable; default this machine, named after its host
  --endpoint NAME=URL     a machine reached through tmag serve; repeatable
  --credential NAME=FILE  the file holding an endpoint's tmag auth credential
  --addr ADDR             the loopback address to listen on (default 127.0.0.1:7778)
  --push KINDS            the alerts pushed to devices: blocked,finished (default); "" for none
  --push-service HOST     a push service beside the browsers' own; repeatable
  --state-dir DIR         the gateway's devices, read turns and push keys
                          (default $TMAG_CLIENT_STATE_DIR, or $XDG_STATE_HOME/tmag-client)

Auth:
  LABEL                   what to call the device; without one it names itself
  --ttl D                 the credential's life, such as 720h (default: until revoked)
  --json                  print a JSON document

Exit codes: 0 ok, 1 error, 2 usage.
`

var errUsage = errors.New("usage")

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "tmag-client:", err)
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	os.Exit(1)
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("%w: name a command", errUsage)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "auth":
		return auth(args[1:])
	case "--version", "-V", "version":
		fmt.Printf("tmag-client %s (%s, %s)\n", version.Version, version.State, version.HTTP)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("%w: unknown command %q; see tmag-client --help", errUsage, args[0])
}

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse reads flags wherever they come among the arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", errUsage, fs.Name(), err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
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

// addressFile is where serve leaves the address devices pair at, for auth
// pair to print.
const addressFile = "address"

func serve(args []string) error {
	var public, machines, endpoints, credentials, services values
	fs := flags("serve")
	fs.Var(&public, "public", "")
	fs.Var(&machines, "machine", "")
	fs.Var(&endpoints, "endpoint", "")
	fs.Var(&credentials, "credential", "")
	fs.Var(&services, "push-service", "")
	addr := fs.String("addr", "127.0.0.1:7778", "")
	kinds := fs.String("push", "blocked,finished", "")
	dir := fs.String("state-dir", defaultState(), "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: serve takes no argument %q", errUsage, pos[0])
	}
	cfg := gateway.Config{Version: version.Version, Log: log.New(os.Stderr, "", log.Ltime), PushServices: services}
	for _, p := range public {
		u, err := url.Parse(strings.TrimRight(p, "/"))
		if err != nil {
			return fmt.Errorf("%w: --public %s: %w", errUsage, p, err)
		}
		cfg.Public = append(cfg.Public, u)
	}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			cfg.Push = append(cfg.Push, k)
		}
	}
	if cfg.Machines, err = backends(machines, endpoints, credentials); err != nil {
		return err
	}
	if cfg.Devices, err = devices.Open(*dir); err != nil {
		return err
	}
	cfg.StateDir = *dir
	g, err := gateway.New(cfg)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("%w: --addr %s: the gateway listens on loopback; a tunnel reaches it from elsewhere", errUsage, *addr)
	}
	local := ln.Addr().String()
	_ = os.WriteFile(filepath.Join(*dir, addressFile), []byte(strings.TrimSuffix(g.PairURL(local, ""), "/#pair=")+"\n"), 0o600)

	fmt.Printf("tmag-client %s at http://%s", version.Version, local)
	for _, u := range cfg.Public {
		fmt.Printf(", reached at %s", u)
	}
	fmt.Println()
	for _, m := range cfg.Machines {
		fmt.Printf("  following %s: %s\n", m.Name, m.Backend)
	}
	if len(cfg.Public) == 0 {
		fmt.Println("  only this machine reaches the page, and nothing is pushed: give --public an https origin a tunnel serves")
	}
	if list, err := cfg.Devices.Devices(); err == nil && len(list) == 0 {
		inv, err := cfg.Devices.Invite("", 0)
		if err != nil {
			return err
		}
		printInvitation(os.Stdout, g.PairURL(local, inv.Code), inv)
	} else if err == nil {
		fmt.Printf("  %d paired devices; pair another with tmag-client auth pair\n", len(list))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	ran := make(chan struct{})
	go func() { g.Run(ctx); close(ran) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err = <-served:
	}
	stop()
	// Verbs that were asked for finish; streams end with the gateway.
	quit, cancel := context.WithTimeout(context.Background(), 2*time.Minute+5*time.Second)
	defer cancel()
	_ = srv.Shutdown(quit)
	<-ran
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// backends are the machines the flags name, or this machine's tmag.
func backends(commands, endpoints, credentials values) ([]machine.Machine, error) {
	var out []machine.Machine
	for _, spec := range commands {
		name, command, ok := strings.Cut(spec, "=")
		argv := strings.Fields(command)
		if !ok {
			argv = []string{localTmag()}
		} else if len(argv) == 0 {
			return nil, fmt.Errorf("%w: --machine %q: want NAME=COMMAND, such as box='ssh box tmag', or NAME for this machine", errUsage, spec)
		}
		out = append(out, machine.Machine{Name: name, Backend: machine.NewCommand(argv)})
	}
	files := map[string]string{}
	for _, spec := range credentials {
		name, file, ok := strings.Cut(spec, "=")
		if !ok || file == "" {
			return nil, fmt.Errorf("%w: --credential %q: want NAME=FILE", errUsage, spec)
		}
		files[name] = file
	}
	for _, spec := range endpoints {
		name, endpoint, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("%w: --endpoint %q: want NAME=URL", errUsage, spec)
		}
		file, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("%w: --endpoint %s needs --credential %s=FILE", errUsage, name, name)
		}
		delete(files, name)
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		token, err := machine.ReadToken(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		h, err := machine.NewHTTP(endpoint, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errUsage, err)
		}
		out = append(out, machine.Machine{Name: name, Backend: h})
	}
	for name := range files {
		return nil, fmt.Errorf("%w: --credential %s names no --endpoint", errUsage, name)
	}
	if len(out) == 0 {
		out = append(out, machine.Machine{Name: hostname(), Backend: machine.NewCommand([]string{localTmag()})})
	}
	return out, nil
}

// hostname names this machine as a machine name takes it.
func hostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	h = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(h, "-")
	h = strings.Trim(h, "-._")
	if len(h) > 32 {
		h = h[:32]
	}
	if machine.CheckName(h) != nil {
		return "local"
	}
	return h
}

// localTmag is this machine's tmag: on PATH, or where the tmux plugin keeps
// it.
func localTmag() string {
	if p, err := exec.LookPath("tmag"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{os.Getenv("TMUX_PLUGIN_MANAGER_PATH"), filepath.Join(home, ".tmux", "plugins"), filepath.Join(home, ".config", "tmux", "plugins")} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, "tmux-agent-experience", "bin", "tmag")
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return "tmag"
}

func printInvitation(w io.Writer, link string, inv devices.Invitation) {
	fmt.Fprintf(w, "\nPair a device: scan this code, or open %s\n", link)
	if code, err := qr.Encode(link); err == nil {
		fmt.Fprint(w, code.Terminal())
	}
	fmt.Fprintf(w, "or enter %s on the page. It pairs one device, before %s.\n", inv.Code, inv.Expires.Local().Format("15:04"))
	fmt.Fprintln(w, "On an iPhone, add the page to the Home Screen first, and pair from there.")
}

func auth(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: auth pair, list or revoke", errUsage)
	}
	verb := args[0]
	fs := flags("auth " + verb)
	asJSON := fs.Bool("json", false, "")
	life := fs.Duration("ttl", 0, "")
	dir := fs.String("state-dir", defaultState(), "")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	store, err := devices.Open(*dir)
	if err != nil {
		return err
	}
	switch verb {
	case "pair":
		if len(pos) > 1 {
			return fmt.Errorf("%w: auth pair takes one label", errUsage)
		}
		label := strings.Join(pos, "")
		inv, err := store.Invite(label, *life)
		if err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		link := ""
		if b, err := os.ReadFile(filepath.Join(*dir, addressFile)); err == nil {
			link = strings.TrimSpace(string(b)) + "/#pair=" + inv.Code
		}
		if *asJSON {
			return emit(map[string]any{"invitation": inv, "url": link})
		}
		if link == "" {
			fmt.Printf("%s\nEnter it on the gateway's page before %s; it pairs one device.\n", inv.Code, inv.Expires.Local().Format("15:04"))
			return nil
		}
		printInvitation(os.Stdout, link, inv)
	case "list":
		if len(pos) > 0 {
			return fmt.Errorf("%w: auth list takes no argument", errUsage)
		}
		list, err := store.Devices()
		if err != nil {
			return err
		}
		if *asJSON {
			if list == nil {
				list = []devices.Device{}
			}
			for i := range list {
				if list[i].Push != nil {
					list[i].Push = &devices.Subscription{Endpoint: list[i].Push.Endpoint}
				}
			}
			return emit(map[string]any{"devices": list})
		}
		for _, d := range list {
			until, push := "until revoked", ""
			if d.Expires != nil {
				until = "until " + d.Expires.Local().Format(time.DateTime)
			}
			if d.Push != nil {
				push = "\tnotifications on"
			}
			fmt.Printf("%s\t%s\tpaired %s, %s%s\n", d.ID, d.Label, d.Created.Local().Format(time.DateTime), until, push)
		}
	case "revoke":
		if len(pos) != 1 {
			return fmt.Errorf("%w: auth revoke takes a device id", errUsage)
		}
		if err := store.Revoke(pos[0]); err != nil {
			return err
		}
		if *asJSON {
			return emit(map[string]any{"revoked": pos[0]})
		}
		fmt.Printf("Unpaired %s; its page closes and it gets no more notifications.\n", pos[0])
	default:
		return fmt.Errorf("%w: auth %s: expected pair, list or revoke", errUsage, verb)
	}
	return nil
}

func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
