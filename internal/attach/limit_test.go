package attach

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

func TestFromPaneReadsOnlyAttachmentsTmagWrites(t *testing.T) {
	valid := Attachment{Agent: proc.Claude, Session: "c1", PID: 101, Start: "s%1", Evidence: Process}
	// Each spoils the attachment; those that leave a change marked leave one
	// tmag cannot read, which is not cleared as broken.
	for name, c := range map[string]struct {
		spoil  func(o map[string]string)
		marked bool
	}{
		"unknown agent":     {func(o map[string]string) { o[optAgent] = "aider" }, false},
		"path as session":   {func(o map[string]string) { o[optSession] = "../c1" }, false},
		"oversized session": {func(o map[string]string) { o[optSession] = strings.Repeat("c", 200) }, false},
		"unknown change":    {func(o map[string]string) { o[optChange] = eventlog.Exit }, true},
		"invalid session, change marked": {func(o map[string]string) {
			o[optSession], o[optChange] = "../c1", eventlog.Detach
		}, true},
		"replaced, no id": {func(o map[string]string) {
			o[optChange], o[optReplaced] = eventlog.Attach, proc.Codex
		}, true},
		"replaced by no session": {func(o map[string]string) {
			o[optChange], o[optReplaced] = eventlog.Attach, proc.Codex+" "+strings.Repeat("x", 129)
		}, true},
	} {
		o := stored(valid)
		c.spoil(o)
		p := tmux.Pane{Server: sock, ID: "%1", Options: o}
		if a, ok := FromPane(p); ok {
			t.Errorf("%s read as %+v", name, a)
		}
		if unreadable(p) != c.marked {
			t.Errorf("%s: unreadable = %v", name, !c.marked)
		}
		if err := markerError(p); strings.Contains(err.Error(), "../") || strings.Contains(err.Error(), "xxxx") {
			t.Errorf("%s: %v", name, err)
		}
	}
	o := stored(valid)
	o[optChange], o[optReplaced] = eventlog.Attach, proc.Codex+" "+threadA
	if a, ok := FromPane(tmux.Pane{Server: sock, ID: "%1", Options: o}); !ok || a.Replaced != proc.Codex+" "+threadA || !a.same(valid) {
		t.Fatalf("a replacing attachment read as %+v, %v", a, ok)
	}
}

// largest is the record of the given shape with the most n that still fits
// the record limit, as stored and read back.
func largest(t *testing.T, store session.Store, shape func(n int) session.Record) session.Record {
	t.Helper()
	fits := func(n int) bool {
		b, err := json.Marshal(shape(n))
		if err != nil {
			t.Fatal(err)
		}
		return len(b) <= session.MaxRecordBytes
	}
	lo, hi := 0, 1
	for fits(hi) {
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		if mid := (lo + hi) / 2; fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	rec := shape(lo)
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(rec.Agent, rec.ID)
	if err != nil || !ok {
		t.Fatalf("stored record unreadable: %v %v", ok, err)
	}
	return got
}

// An Attach event reports the session as read under its lock, within the
// record limit, beside a pane and its evidence. Whatever such a record holds,
// the event stays within twice its size, far inside the event log's limit.
func TestAttachEventOfAnyReadableRecordFits(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	base := func() session.Record { return session.Record{Agent: proc.Codex, ID: threadA, Main: adapter.Idle} }
	shapes := map[string]func(n int) session.Record{
		"minimal requests": func(n int) session.Record {
			r := base()
			r.Requests = make([]session.Request, n)
			return r
		},
		"escaped requests": func(n int) session.Record {
			r := base()
			for range n {
				r.Requests = append(r.Requests, session.Request{ID: "<", Prompt: adapter.Prompt{Agent: "<", Tool: "<"}})
			}
			return r
		},
		"running subagents": func(n int) session.Record {
			r := base()
			for range n {
				r.Subagents, r.Background = append(r.Subagents, "<"), append(r.Background, "<")
			}
			return r
		},
		"paths and turn": func(n int) session.Record {
			r := base()
			r.Cwd, r.Transcript, r.Turn = strings.Repeat("<", n), strings.Repeat("\x80", n), strings.Repeat("\x01", n)
			return r
		},
	}
	pane := &eventlog.PaneRef{Server: "/" + strings.Repeat("s", 4095), ID: "%2147483647"}
	for name, shape := range shapes {
		rec := largest(t, store, shape)
		ev := eventlog.Event{Agent: rec.Agent, Session: rec.ID, Event: eventlog.Attach, Pane: pane, Evidence: TitleID}
		rec.Report(&ev)
		n, err := eventlog.Admit(ev)
		if err != nil || n > 2*session.MaxRecordBytes+64<<10 {
			t.Fatalf("%s: the Attach event of a readable record is %d bytes: %v", name, n, err)
		}
	}
}
