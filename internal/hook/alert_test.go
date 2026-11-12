package hook

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// hookWorld is a state directory and its log, with no tmux server: a change
// there alerts with nobody at a terminal.
func hookWorld(t *testing.T) (string, *eventlog.Log) {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	dir := t.TempDir()
	return dir, eventlog.Open(dir)
}

// runClaudeHook runs one hook of a session as `tmag hook claude` does.
func runClaudeHook(t *testing.T, log *eventlog.Log, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunContext(ctx, Input{Agent: proc.Claude, Log: log, Payload: strings.NewReader(payload)})
	if err != nil {
		t.Fatalf("hook %s: %v", payload, err)
	}
}

// logged returns what the whole log holds.
func logged(t *testing.T, log *eventlog.Log) []eventlog.Event {
	t.Helper()
	events, _, err := log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// A finished turn is logged as an Alert event after the Stop that ended it,
// reporting the turn and nobody at a terminal; it reports no session state.
func TestAlertEventReportsAFinishedTurn(t *testing.T) {
	dir, log := hookWorld(t)
	ctx := context.Background()
	rec := session.Record{Agent: proc.Claude, ID: "finished", Main: adapter.Working, Turn: "turn-1", Since: time.Unix(10, 0).UTC()}
	if err := (session.Store{Dir: dir}).Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	runClaudeHook(t, log, `{"session_id":"finished","hook_event_name":"Stop","prompt_id":"turn-1","cwd":"/w"}`)

	events := logged(t, log)
	if len(events) != 2 || events[0].Event != "Stop" || events[1].Event != eventlog.Alert {
		t.Fatalf("events = %+v", events)
	}
	ev := events[1]
	if ev.Agent != proc.Claude || ev.Session != "finished" || ev.Alert == nil {
		t.Fatalf("alert = %+v", ev)
	}
	a := ev.Alert
	if a.Kind != "finished" || a.Turn != "turn-1" || a.Present || a.Request != nil {
		t.Fatalf("alert = %+v", a)
	}
	if ev.State != "" || ev.Kind != "" || ev.Turn != "" || ev.Cwd != "" || ev.Since != nil || ev.Requests != nil {
		t.Fatalf("alert reports the session: %+v", ev)
	}
}

// A request that opens is logged as an Alert event after the hook that
// opened it, naming the request the notify command is given.
func TestAlertEventReportsABlockedRequest(t *testing.T) {
	dir, log := hookWorld(t)
	runClaudeHook(t, log, `{"session_id":"blocked","hook_event_name":"PermissionRequest","prompt_id":"turn-2","tool_name":"Bash","tool_input":{"command":"ls"}}`)

	rec, ok, err := (session.Store{Dir: dir}).Get(context.Background(), proc.Claude, "blocked")
	if err != nil || !ok || len(rec.Requests) != 1 {
		t.Fatalf("record = %+v, %v, %v", rec, ok, err)
	}
	events := logged(t, log)
	if len(events) != 2 || events[0].Event != "PermissionRequest" || events[0].State != adapter.Blocked || events[1].Event != eventlog.Alert {
		t.Fatalf("events = %+v", events)
	}
	a := events[1].Alert
	if a == nil || a.Kind != "blocked" || a.Present || a.Turn != "" {
		t.Fatalf("alert = %+v", a)
	}
	q := rec.Requests[0]
	if a.Request == nil || *a.Request != (eventlog.AlertRequest{ID: q.ID, Kind: q.Prompt.RequestKind(), Tool: "Bash"}) {
		t.Fatalf("alert request = %+v, request = %+v", a.Request, q)
	}
}

// A change no alert names is logged alone.
func TestOnlyAlertingChangesAreLogged(t *testing.T) {
	_, log := hookWorld(t)
	runClaudeHook(t, log, `{"session_id":"quiet","hook_event_name":"UserPromptSubmit","prompt_id":"turn","cwd":"/w"}`)
	runClaudeHook(t, log, `{"session_id":"quiet","hook_event_name":"PostToolUse","prompt_id":"turn","tool_name":"Bash","tool_use_id":"call"}`)
	for _, ev := range logged(t, log) {
		if ev.Event == eventlog.Alert {
			t.Fatalf("a change that does not alert logged %+v", ev)
		}
	}
}

// presence is the person's input on a server that shows the session, within
// notify.Present.
func TestPresent(t *testing.T) {
	now := time.Now()
	recent := func(tmux.Server) (time.Time, bool) { return now.Add(-time.Second), true }
	stale := func(tmux.Server) (time.Time, bool) { return now.Add(-2 * notify.Present), true }
	none := func(tmux.Server) (time.Time, bool) { return time.Time{}, false }
	main := tmux.Pane{Server: "/tmp/tmux-1000/default", ID: "%1"}
	other := tmux.Pane{Server: "/tmp/tmux-1000/work", ID: "%2"}
	for _, c := range []struct {
		name  string
		shown []tmux.Pane
		last  func(tmux.Server) (time.Time, bool)
		want  bool
	}{
		{"input at a server showing the session", []tmux.Pane{main}, recent, true},
		{"input too old", []tmux.Pane{main}, stale, false},
		{"no client ever typed", []tmux.Pane{main}, none, false},
		{"a session no pane shows", nil, recent, false},
		{"input on another server", []tmux.Pane{main, other}, func(s tmux.Server) (time.Time, bool) {
			return now.Add(-time.Second), s == (tmux.Server{Socket: other.Server})
		}, true},
	} {
		if got := present(c.shown, c.last); got != c.want {
			t.Errorf("%s: present = %v, want %v", c.name, got, c.want)
		}
	}
}

// One tmux call per distinct server: a hook's cost follows the servers that
// show the session, not the panes.
func TestPresentAsksEachServerOnce(t *testing.T) {
	var asked []string
	last := func(s tmux.Server) (time.Time, bool) {
		asked = append(asked, s.Socket)
		return time.Time{}, false
	}
	present([]tmux.Pane{{Server: "/a"}, {Server: "/a"}, {Server: "/b"}}, last)
	if want := []string{"/a", "/b"}; !slices.Equal(asked, want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
}
