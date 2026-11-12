package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTmag is a tmag that prints what it was given, refuses a busy target,
// and watches with one snapshot before the machine goes away.
const fakeTmag = `#!/bin/sh
case "$1" in
watch)
  echo '{"schema":"tmag/v2","type":"snapshot","seq":7,"sessions":[{"id":"s1","agent":"claude","state":"idle"}],"panes":[]}'
  echo '{"schema":"tmag/v2","type":"event","event":{"seq":8,"agent":"claude","session":"s1","event":"UserPromptSubmit","state":"working"}}'
  echo "since=$3" >&2
  echo 'client_loop: send disconnect: Broken pipe' >&2
  exit 255 ;;
send)
  text=$(cat)
  if [ "$4" = busy ]; then
    echo '{"schema":"tmag/v2","error":{"code":"refused:busy","message":"busy is working"}}'
    echo 'tmag: busy is working' >&2
    echo 'next: wait for it, then send again' >&2
    exit 3
  fi
  printf '{"schema":"tmag/v2","args":"%s","text":"%s"}\n' "$*" "$text" ;;
*)
  printf '{"schema":"tmag/v2","argc":%d}\n' "$#"
  for a in "$@"; do printf '%s\n' "$a" >> "$ARGS_FILE"; done ;;
esac
`

// fakeSSH runs its command as a remote shell would: the words after the
// host joined, then split again.
const fakeSSH = `#!/bin/sh
shift
exec sh -c "$*"
`

func install(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestACommandCarriesVerbs(t *testing.T) {
	tmag := install(t, "tmag", fakeTmag)
	c := NewCommand([]string{tmag})
	doc, err := c.Call(context.Background(), Op{Verb: "send", Target: "s1", Text: "review it"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), `"args":"send --json -- s1 -","text":"review it"`) {
		t.Fatalf("send ran as %s", doc)
	}
	_, err = c.Call(context.Background(), Op{Verb: "send", Target: "busy", Text: "x"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "refused:busy" || e.Message != "busy is working" || e.Next != "wait for it, then send again" {
		t.Fatalf("refusal: %#v", err)
	}
	gone := NewCommand([]string{install(t, "ssh", "#!/bin/sh\necho 'ssh: connect to host box port 22: Connection refused' >&2\nexit 255\n"), "box", "tmag"})
	_, err = gone.Call(context.Background(), Op{Verb: "interrupt", Target: "s1"})
	if !errors.As(err, &e) || e.Code != Unreachable || !strings.Contains(e.Message, "Connection refused") {
		t.Fatalf("unreachable: %#v", err)
	}
}

func TestArgumentsCrossSSHIntact(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", args)
	tmag := install(t, "tmag", fakeTmag)
	c := NewCommand([]string{install(t, "ssh", fakeSSH), "box", tmag})
	note := `don't run "rm -rf $HOME"; use * instead`
	if _, err := c.Call(context.Background(), Op{Verb: "answer", Target: "s1", Request: "r1", Decision: "deny", Message: note, Answers: map[string]string{"Which?": "A b"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(args)
	want := "answer\n--json\n--message=" + note + "\n--answers={\"Which?\":\"A b\"}\n--\ns1\nr1\ndeny\n"
	if string(b) != want {
		t.Fatalf("tmag got\n%s\nwant\n%s", b, want)
	}
}

func TestACommandWatchEndsWithTheMachine(t *testing.T) {
	c := NewCommand([]string{install(t, "tmag", fakeTmag)})
	var docs []Doc
	err := c.Watch(context.Background(), 41, func(d Doc) error { docs = append(docs, d); return nil })
	if len(docs) != 2 || docs[0].Type != "snapshot" || docs[0].Seq != 7 || docs[1].Event.State != "working" {
		t.Fatalf("docs %+v", docs)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != Unreachable || e.Message != "client_loop: send disconnect: Broken pipe" {
		t.Fatalf("end: %#v", err)
	}
	stop := errors.New("enough")
	if err := c.Watch(context.Background(), -1, func(Doc) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("stopped: %v", err)
	}
}

// serve answers as tmag serve does.
func serve(t *testing.T, silent bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"schema":"tmag/http/v2","error":{"code":"unauthorized","message":"credential is invalid"}}`)
			return
		}
		switch r.URL.Path {
		case "/v2/read":
			fmt.Fprintf(w, `{"schema":"tmag/v2","query":%q}`, r.URL.RawQuery)
		case "/v2/act":
			b, _ := io.ReadAll(r.Body)
			var a map[string]any
			_ = json.Unmarshal(b, &a)
			if a["verb"] == "interrupt" {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"schema":"tmag/v2","error":{"code":"refused:idle","message":"s1 is idle"}}`)
				return
			}
			_, _ = w.Write(b)
		case "/v2/watch":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": watching\n\nid: 7\nevent: doc\ndata: {\"type\":\"snapshot\",\"seq\":7,\"sessions\":[],\"panes\":[]}\n\n")
			fmt.Fprintf(w, "event: ping\ndata: 1\n\n")
			fmt.Fprintf(w, "event: doc\ndata: {\"type\":\"panes\",\"panes\":[{\"server\":\"/tmp/s\",\"id\":\"%%1\",\"target\":\"w:1.0\"}]}\n\n")
			w.(http.Flusher).Flush()
			if silent {
				<-r.Context().Done()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnEndpointCarriesVerbs(t *testing.T) {
	srv := serve(t, false)
	h, err := NewHTTP(srv.URL+"/", "tok\n")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := h.Call(context.Background(), Op{Verb: "read", Target: "s1", Last: true})
	if err != nil || !strings.Contains(string(doc), `"query":"last=1&target=s1"`) {
		t.Fatalf("read: %s %v", doc, err)
	}
	doc, err = h.Call(context.Background(), Op{Verb: "send", Target: "s1", Text: "go on"})
	if err != nil || string(doc) != `{"target":"s1","text":"go on","verb":"send"}` {
		t.Fatalf("send: %s %v", doc, err)
	}
	var e *Error
	if _, err := h.Call(context.Background(), Op{Verb: "interrupt", Target: "s1"}); !errors.As(err, &e) || e.Code != "refused:idle" {
		t.Fatalf("refusal: %#v", err)
	}
	wrong, _ := NewHTTP(srv.URL, "other")
	if _, err := wrong.Call(context.Background(), Op{Verb: "read", Target: "s1", Lines: 5}); !errors.As(err, &e) || e.Code != "unauthorized" {
		t.Fatalf("unauthorized: %#v", err)
	}
}

func TestAnEndpointWatchNoticesSilence(t *testing.T) {
	h, _ := NewHTTP(serve(t, true).URL, "tok")
	h.silence = 200 * time.Millisecond
	var docs []Doc
	err := h.Watch(context.Background(), -1, func(d Doc) error { docs = append(docs, d); return nil })
	if len(docs) != 2 || docs[0].Seq != 7 || docs[1].Panes[0].Target != "w:1.0" {
		t.Fatalf("docs %+v", docs)
	}
	var e *Error
	if !errors.As(err, &e) || e.Message != "tmag serve went silent" {
		t.Fatalf("end: %#v", err)
	}
}

func TestEndpointsAreHTTPSOrLoopback(t *testing.T) {
	for _, ok := range []string{"https://box.example.net", "http://127.0.0.1:7777", "http://localhost:7777", "http://[::1]:7777"} {
		if _, err := NewHTTP(ok, "t"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://box.example.net", "ftp://box", "https://u@box", "https://box/?x=1", "box"} {
		if _, err := NewHTTP(bad, "t"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := NewHTTP("https://box", " "); err == nil {
		t.Error("an empty credential accepted")
	}
}

func TestCredentialFiles(t *testing.T) {
	for in, want := range map[string]string{"tok\n": "tok", `{"schema":"tmag/v2","credential":{"id":"a","token":"tok"}}`: "tok"} {
		if got, err := ReadToken([]byte(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ReadToken([]byte(`{"credential":{}}`)); err == nil {
		t.Error("a document without a token accepted")
	}
}
