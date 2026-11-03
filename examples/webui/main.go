// Command webui is a minimal web page over `tmag watch`.
//
// It exists to check that the watch stream is all a client needs: the page
// rebuilds every session and agent pane from the stream alone, resumes with
// `--since` after a disconnect, and can compare what it rebuilt with
// `tmag list --json`. The server only relays: each page connection runs its
// own `tmag watch` and gets its lines unchanged as server-sent events.
//
// It is an example consumer of the tmag/v1 contract, not part of tmag. It
// runs only while you run it, listens on the loopback interface by default
// and never sends anything to an agent.
//
//	go run ./examples/webui                      # tmag on PATH
//	go run ./examples/webui -- ~/.tmux/plugins/tmux-agent-experience/bin/tmag
//	go run ./examples/webui -- ssh host tmag     # tmag on another machine
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

//go:embed index.html
var page []byte

// maxLine bounds one JSON document of the stream; a snapshot of many
// sessions and panes is the largest.
const maxLine = 16 << 20

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: webui [-addr ADDR] [-- TMAG-COMMAND...]\n\n"+
			"TMAG-COMMAND runs tmag, default \"tmag\"; for example \"ssh host tmag\".\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	tmag := flag.Args()
	if len(tmag) == 0 {
		tmag = []string{"tmag"}
	}
	if err := run(*addr, tmag); err != nil {
		log.Fatal(err)
	}
}

func run(addr string, tmag []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s := server{tmag: tmag, loopback: isLoopback(ln.Addr())}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /watch", s.watch)
	mux.HandleFunc("GET /list", s.list)
	srv := &http.Server{Handler: s.guard(mux), BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("serving http://%s for %q", ln.Addr(), tmag)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	tmag     []string // the command that runs tmag
	loopback bool     // listening on the loopback interface only
}

// guard refuses requests addressed to another host name while the server
// listens on loopback, so that a web page elsewhere cannot read the sessions
// through DNS rebinding.
func (s server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.loopback {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

func (s server) command(ctx context.Context, args ...string) *exec.Cmd {
	argv := append(append([]string{}, s.tmag...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Let tmag (or ssh) end on its own terms, then insist.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = os.Stderr
	return cmd
}

func (server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// watch relays `tmag watch [--since SEQ]` as server-sent events, one line
// per message. When tmag exits, a final "end" event carries why.
func (s server) watch(w http.ResponseWriter, r *http.Request) {
	args := []string{"watch"}
	if v := r.URL.Query().Get("since"); v != "" {
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "since: not an integer", http.StatusBadRequest)
			return
		}
		args = append(args, "--since", v)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	cmd := s.command(r.Context(), args...)
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprint(w, ": watching\n\n")
	flusher.Flush()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", sc.Bytes()); err != nil {
			break
		}
		flusher.Flush()
	}
	reason := "tmag watch exited"
	if err := sc.Err(); err != nil {
		reason = err.Error()
		_ = cmd.Cancel()
	}
	if err := cmd.Wait(); err != nil && r.Context().Err() == nil {
		reason = err.Error()
	}
	_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", strconv.Quote(reason))
	flusher.Flush()
}

// list returns `tmag list --json`, for comparing with the state the page
// rebuilt from the stream.
func (s server) list(w http.ResponseWriter, r *http.Request) {
	out, err := s.command(r.Context(), "list", "--json").Output()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		if len(out) == 0 {
			_, _ = fmt.Fprintf(w, `{"error":{"code":"error","message":%s}}`, strconv.Quote(err.Error()))
			return
		}
	}
	_, _ = w.Write(out)
}
