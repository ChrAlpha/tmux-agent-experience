// tmag-client hosts the independent web client and optional command gateway.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tmag-client/internal/gateway"
	"tmag-client/internal/httpclient"
)

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }
func main() {
	if len(os.Args) > 1 && os.Args[1] == "http" {
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "HTTP worker needs an endpoint, credential file and operation")
			os.Exit(2)
		}
		b, err := os.ReadFile(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot read endpoint credential")
			os.Exit(1)
		}
		token := strings.TrimSpace(string(b))
		if strings.HasPrefix(token, "{") {
			var issued struct{ Credential struct{ Token string } }
			if json.Unmarshal(b, &issued) != nil {
				os.Exit(1)
			}
			token = issued.Credential.Token
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		code := httpclient.Run(ctx, os.Args[2], token, os.Args[4:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tmag-client:", err)
		os.Exit(1)
	}
}
func run() error {
	var machines, endpoints, credentials values
	fs := flag.NewFlagSet("tmag-client", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7778", "loopback HTTP listen address")
	owner := fs.String("owner", "", "owner identity proven by a tunnel")
	tailscale := fs.String("tailscale", "", "comma-separated Tailscale host names")
	access := fs.String("access", "", "comma-separated Cloudflare Access host names")
	team := fs.String("access-team", "", "Cloudflare Access team")
	audience := fs.String("access-aud", "", "Cloudflare Access audience")
	dir := fs.String("state-dir", defaultState(), "gateway's persistent authorization directory")
	fs.Var(&endpoints, "endpoint", "NAME=URL for a tmag/http/v1 endpoint; repeatable")
	fs.Var(&credentials, "credential", "NAME=FILE holding an endpoint credential; repeatable")
	fs.Var(&machines, "machine", "NAME=COMMAND that runs tmag; repeatable")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if len(machines)+len(endpoints) == 0 {
		machines = values{"local=tmag"}
	}
	cfg := gateway.Config{Owner: *owner, Tailscale: split(*tailscale), Access: gateway.Access{Hosts: split(*access), Team: *team, Audience: *audience}, Version: "client-dev", Log: log.New(os.Stderr, "", log.Ltime)}
	for _, m := range machines {
		parsed, err := gateway.ParseMachine(m)
		if err != nil {
			return err
		}
		cfg.Machines = append(cfg.Machines, parsed)
	}

	files := map[string]string{}
	for _, spec := range credentials {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			return errors.New("credential must be NAME=FILE")
		}
		if _, exists := files[name]; exists {
			return errors.New("duplicate credential mapping")
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
			return errors.New("endpoint must be NAME=URL")
		}
		if _, err := httpclient.Endpoint(endpoint); err != nil {
			return err
		}
		m, err := gateway.ParseMachine(name + "=tmag")
		if err != nil {
			return err
		}
		file := files[name]
		if file == "" {
			return errors.New("endpoint needs a --credential mapping")
		}
		delete(files, name)
		m.Argv = []string{self, "http", endpoint, file}
		cfg.Machines = append(cfg.Machines, m)
	}
	if len(files) > 0 {
		return errors.New("credential mapping has no endpoint")
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	tokenPath := filepath.Join(*dir, "gateway.token")
	b, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		b = []byte(hex.EncodeToString(raw))
		err = os.WriteFile(tokenPath, b, 0600)
	}
	if err != nil {
		return err
	}
	cfg.Token = strings.TrimSpace(string(b))
	if len(cfg.Token) < 32 {
		return errors.New("invalid gateway authorization file")
	}
	handler, err := gateway.New(cfg)
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
		return errors.New("the client gateway listens on loopback")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Fprintf(os.Stdout, "tmag-client at http://%s/#token=%s\n", ln.Addr(), cfg.Token)
	if len(cfg.Tailscale)+len(cfg.Access.Hosts) > 0 {
		fmt.Fprintf(os.Stdout, "Pair a browser with %s\n", handler.PairingCode())
	}
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
	stop()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func defaultState() string {
	if p := os.Getenv("XDG_STATE_HOME"); p != "" {
		return filepath.Join(p, "tmag-client")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tmag-client")
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
