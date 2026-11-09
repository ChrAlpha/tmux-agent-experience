package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// recordPath is where s keeps the record of claude session s1.
func recordPath(s Store) string {
	return filepath.Join(s.Dir, "sessions", proc.Claude, "s1.json")
}

func fileOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func headOf(t *testing.T, log *eventlog.Log) eventlog.Cursor {
	t.Helper()
	c, err := log.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func eventsOf(t *testing.T, log *eventlog.Log) []eventlog.Event {
	t.Helper()
	all, _, err := log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// limited fails unless err refuses part at its limit, naming the session but
// none of what the part holds.
func limited(t *testing.T, err error, part string, limit int) {
	t.Helper()
	var refused *LimitError
	if !errors.As(err, &refused) || refused.Part != part || refused.Limit != limit || refused.Size <= limit {
		t.Fatalf("err = %v, want the %s refused at %d bytes", err, part, limit)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "limit_exceeded: ") || strings.Contains(msg, "xxxx") || strings.Contains(msg, "<<<<") || len(msg) > 200 {
		t.Fatalf("message %q", msg)
	}
}

// padTo grows rec's one finished-call id, which a record holds once and no
// event reports, until size counts exactly MaxRecordBytes.
func padTo(t *testing.T, rec Record, size func(Record) int) Record {
	t.Helper()
	rec.Done = []string{"x"}
	rec.Done = []string{strings.Repeat("x", 1+MaxRecordBytes-size(rec))}
	if got := size(rec); got != MaxRecordBytes {
		t.Fatalf("padded to %d bytes, want %d", got, MaxRecordBytes)
	}
	return rec
}

// plus is rec with one more byte in its padding.
func plus(rec Record) Record {
	rec.Done = []string{rec.Done[0] + "x"}
	return rec
}

// stamped is ev without what Append assigns.
func stamped(ev eventlog.Event) eventlog.Event {
	ev.Seq, ev.Time = 0, time.Time{}
	return ev
}

func TestPutAdmitsExactlyTheReadersLimit(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	path := recordPath(s)
	onDisk := func(r Record) int {
		if err := s.Put(r); err != nil {
			t.Fatal(err)
		}
		return len(fileOf(t, path))
	}
	rec := padTo(t, Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle}, onDisk)
	got, ok, err := s.Get(proc.Claude, "s1")
	if err != nil || !ok || got.Done[0] != rec.Done[0] {
		t.Fatalf("a record of exactly the limit is unreadable: %v %v", ok, err)
	}

	before := fileOf(t, path)
	limited(t, s.Put(plus(rec)), partRecord, MaxRecordBytes)
	if fileOf(t, path) != before {
		t.Fatal("a refused record replaced the stored one")
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "sessions", proc.Claude, ".tmag-tmp-*")); len(left) != 0 {
		t.Fatalf("a refused record left %v", left)
	}
}

// Encoding is what counts: HTML characters and control bytes encode to six
// bytes each, and an invalid UTF-8 byte to at least the three of a
// replacement character (six where the toolchain escapes it).
func TestPutCountsEncodedBytes(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for c, least := range map[string]int{"<": 6, "\x01": 6, "\x80": 3} {
		rec := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Turn: strings.Repeat(c, MaxRecordBytes/least+1)}
		limited(t, s.Put(rec), partRecord, MaxRecordBytes)
		if _, found, err := s.Get(proc.Claude, "s1"); found || err != nil {
			t.Fatalf("a refused record was stored: %v %v", found, err)
		}
	}
}

func TestLimitErrorNamesOnlyAValidSession(t *testing.T) {
	e := &LimitError{Agent: proc.Claude, ID: "../" + strings.Repeat("x", 300), Part: partRecord, Size: 9, Limit: 8}
	if msg := e.Error(); msg != "limit_exceeded: session record encodes to 9 bytes, over 8" {
		t.Fatalf("message %q", msg)
	}
}

// Logged, a record holds its cwd twice: a change whose record fits marked
// can still be refused for the record it leaves.
func TestSaveRefusesAFinalRecordOverTheLimit(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	rec, _ := Apply(Record{Agent: proc.Claude, ID: "s1", Cwd: "/a"}, adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
	rec, err := s.Save(ctx, log, rec, "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	path := recordPath(s)
	before, head := fileOf(t, path), headOf(t, log)

	moved := rec
	moved.Cwd = "/" + strings.Repeat("d", MaxRecordBytes/2+1024)
	_, err = s.Save(ctx, log, moved, "PreToolUse")
	limited(t, err, partFinal, MaxRecordBytes)
	if fileOf(t, path) != before || headOf(t, log) != head {
		t.Fatal("a refused change was published")
	}

	moved.Cwd = "/b"
	if _, err := s.Save(ctx, log, moved, "PreToolUse"); err != nil {
		t.Fatalf("the next change: %v", err)
	}
	if all := eventsOf(t, log); len(all) != 2 || all[1].Cwd != "/b" {
		t.Fatalf("events %+v", all)
	}
}

// Marked, a delivery's record keeps the delivery's event too: a change whose
// record fits logged can still be refused for its mark.
func TestReportRefusesAPendingRecordOverTheLimit(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	rec, _ := Apply(Record{Agent: proc.Claude, ID: "s1", Cwd: "/a"}, adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
	rec, err := s.Save(ctx, log, rec, "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	path := recordPath(s)
	// Settled, the record is what Report would leave after an interrupt
	// that changed nothing it reports.
	rec = padTo(t, rec, func(r Record) int {
		if err := s.Put(r); err != nil {
			t.Fatal(err)
		}
		return len(fileOf(t, path))
	})
	before, head := fileOf(t, path), headOf(t, log)
	ev := eventlog.Event{Event: eventlog.Halt, Via: "pane", Pane: &eventlog.PaneRef{Server: "/tmp/tmux-1000/default", ID: "%1"}}
	_, err = s.Report(ctx, log, rec, &ev)
	limited(t, err, partPending, MaxRecordBytes)
	if fileOf(t, path) != before || headOf(t, log) != head || ev.Seq != 0 {
		t.Fatal("a refused delivery was published")
	}
}

// The record limit keeps every session event far below the event log's; a
// lower event bound reaches the refusal of the event itself.
func TestPrepareRefusesAnEventOverItsLimit(t *testing.T) {
	l := limits{record: MaxRecordBytes, event: 512}
	rec := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Cwd: strings.Repeat("c", 600)}
	_, err := l.prepare(rec, eventlog.Event{Event: "Stop"}, false)
	limited(t, err, partEvent, 512)
	_, err = l.prepare(rec, eventlog.Event{Event: eventlog.Halt}, true)
	limited(t, err, partEvent, 512)
	_, err = l.prepareEnd(rec, eventlog.Event{Event: eventlog.Exit})
	limited(t, err, partEvent, 512)
	limited(t, l.admit(eventlog.Event{Agent: proc.Claude, Session: "s1", Event: eventlog.Send, Cwd: rec.Cwd}), partEvent, 512)
}

// A change marked at the limit settles after its writer dies, before or
// after logging it, into what the writer would have published.
func TestSettleFinishesAChangeMarkedAtTheLimit(t *testing.T) {
	saved := func(t *testing.T, s Store, log *eventlog.Log) Record {
		rec, _ := Apply(Record{Agent: proc.Claude, ID: "s1", Cwd: "/a"}, adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
		rec, err := s.Save(context.Background(), log, rec, "SessionStart")
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	halt := eventlog.Event{Event: eventlog.Halt, Via: "pane", Pane: &eventlog.PaneRef{Server: "/tmp/tmux-1000/default", ID: "%1"}}
	cases := []struct {
		name string
		// change is the change, with its prepared publications, made to the
		// stored session.
		change func(rec Record) (Prepared, error)
		// base is the session before the change.
		base func(t *testing.T, s Store, log *eventlog.Log) Record
	}{
		{"save", func(rec Record) (Prepared, error) {
			rec.Main = adapter.Working
			return PrepareSave(rec, "UserPromptSubmit")
		}, saved},
		// Invalid UTF-8 decodes to U+FFFD: both subagents run once stored,
		// and the change is admitted as it will be repaired.
		{"save with colliding ids", func(rec Record) (Prepared, error) {
			rec.Subagents, rec.Background = []string{"a\x80", "a\x81"}, []string{"a\x80"}
			return PrepareSave(rec, "SubagentStart")
		}, saved},
		{"report", func(rec Record) (Prepared, error) {
			rec.Halted = t0
			return PrepareReport(rec, halt)
		}, saved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			// The change's larger record is padded to the limit.
			var padded Record
			{
				dir := t.TempDir()
				base := c.base(t, Store{Dir: dir}, eventlog.Open(dir))
				padded = padTo(t, base, func(r Record) int {
					p, err := c.change(r)
					if err != nil {
						t.Fatal(err)
					}
					return max(len(p.marked), len(p.final))
				})
				if _, err := c.change(plus(padded)); !errors.As(err, new(*LimitError)) {
					t.Fatalf("one byte more: %v", err)
				}
			}
			run := func(crash func(s Store, log *eventlog.Log, p Prepared)) (string, []eventlog.Event) {
				dir := t.TempDir()
				s, log := Store{Dir: dir}, eventlog.Open(dir)
				c.base(t, s, log)
				p, err := c.change(padded)
				if err != nil {
					t.Fatal(err)
				}
				err = s.Lock(ctx, proc.Claude, "s1", func() error {
					if crash == nil {
						_, _, err := s.Publish(ctx, log, p)
						return err
					}
					crash(s, log, p)
					rec, ok, err := s.Current(ctx, log, proc.Claude, "s1")
					if err == nil && (!ok || rec.Unsettled()) {
						t.Fatalf("Current left %+v, %v", rec, ok)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				final := fileOf(t, recordPath(s))
				if len(final) > MaxRecordBytes {
					t.Fatalf("final record is %d bytes", len(final))
				}
				// Settled, it stays as it is.
				err = s.Lock(ctx, proc.Claude, "s1", func() error {
					_, _, err := s.Current(ctx, log, proc.Claude, "s1")
					return err
				})
				if err != nil || fileOf(t, recordPath(s)) != final {
					t.Fatalf("a settled record changed: %v", err)
				}
				return final, eventsOf(t, log)
			}
			whole, logged := run(nil)
			marked, repaired := run(func(s Store, _ *eventlog.Log, p Prepared) {
				if err := s.write(ctx, p.agent, p.id, p.marked); err != nil {
					t.Fatal(err)
				}
			})
			appended, twice := run(func(s Store, log *eventlog.Log, p Prepared) {
				if err := s.write(ctx, p.agent, p.id, p.marked); err != nil {
					t.Fatal(err)
				}
				ev := *p.event
				if err := log.Append(ctx, &ev); err != nil {
					t.Fatal(err)
				}
			})
			if marked != whole || appended != whole {
				t.Fatal("a repair left another record than the writer would have")
			}
			if len(logged) != 2 || len(repaired) != 2 || len(twice) != 3 {
				t.Fatalf("events: %d, %d, %d", len(logged), len(repaired), len(twice))
			}
			want := stamped(logged[1])
			for _, got := range []eventlog.Event{repaired[1], twice[1], twice[2]} {
				if !reflect.DeepEqual(stamped(got), want) {
					t.Fatalf("repair logged %+v, want %+v", stamped(got), want)
				}
			}
		})
	}
}

// A pending prompt without a request is named before the change is
// admitted, as Settle would name it, so a repair adds nothing.
func TestPrepareNamesAPendingPromptWithoutARequest(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	asked := "PermissionRequest"
	rec := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Working, Turn: "p1", Since: t0, Pending: []adapter.Prompt{{Tool: "Bash"}}}
	saved, err := s.Save(ctx, log, rec, asked)
	if err != nil || len(saved.Requests) != 1 || saved.Requests[0].Prompt != rec.Pending[0] {
		t.Fatalf("Save = %+v, %v", saved.Requests, err)
	}
	if all := eventsOf(t, log); len(all) != 1 || len(all[0].Requests) != 1 || all[0].Requests[0].ID != saved.Requests[0].ID {
		t.Fatalf("events %+v", all)
	}
	err = s.Lock(ctx, proc.Claude, "s1", func() error {
		_, _, err := s.Current(ctx, log, proc.Claude, "s1")
		return err
	})
	if err != nil || len(eventsOf(t, log)) != 1 {
		t.Fatalf("a named prompt was repaired: %v", err)
	}

	// The request counts: named by hand, the same record has the same size.
	final := func(r Record) int {
		p, err := PrepareSave(r, asked)
		if err != nil {
			t.Fatal(err)
		}
		return len(p.final)
	}
	named := rec
	named.Requests = []Request{{ID: "abcdef012345", Prompt: rec.Pending[0], Turn: "p1", Since: t0}}
	named = padTo(t, named, final)
	unnamed := rec
	unnamed.Done = named.Done
	if got := final(unnamed); got != MaxRecordBytes {
		t.Fatalf("unnamed, the record is %d bytes", got)
	}
	_, err = PrepareSave(plus(unnamed), asked)
	limited(t, err, partFinal, MaxRecordBytes)
}

// An ended record keeps only what ends the session: a session ends at any
// size its record reached, and a repair logs the end as decided.
func TestEndFitsWhateverTheRecordsSize(t *testing.T) {
	ctx := context.Background()
	contact := t0.Add(time.Minute)
	// full is a settled session whose record is exactly the limit, most of it
	// its cwd.
	full := func(t *testing.T, s Store, log *eventlog.Log) Record {
		rec, _ := Apply(Record{Agent: proc.Claude, ID: "s1", Cwd: "/" + strings.Repeat("c", MaxRecordBytes/2-4096), LastHookAt: t0},
			adapter.Transition{Action: adapter.Set, State: adapter.Idle}, t0)
		rec, err := s.Save(ctx, log, rec, "SessionStart")
		if err != nil {
			t.Fatal(err)
		}
		path := recordPath(s)
		return padTo(t, rec, func(r Record) int {
			if err := s.Put(r); err != nil {
				t.Fatal(err)
			}
			return len(fileOf(t, path))
		})
	}
	end := func(t *testing.T, crash bool, ev eventlog.Event) (Record, eventlog.Event) {
		dir := t.TempDir()
		s, log := Store{Dir: dir}, eventlog.Open(dir)
		rec := full(t, s, log)
		err := s.Lock(ctx, proc.Claude, "s1", func() error {
			if !crash {
				return s.End(ctx, log, rec, ev)
			}
			p, err := PrepareEnd(rec, ev)
			if err != nil {
				return err
			}
			if err := s.write(ctx, p.agent, p.id, p.marked); err != nil {
				return err
			}
			marker, ok, err := s.Get(proc.Claude, "s1")
			if err != nil || !ok || marker.Ended != ev.Event || len(fileOf(t, recordPath(s))) > MaxRecordBytes/2+1024 {
				t.Fatalf("ended record %v %v", ok, err)
			}
			_, ok, err = s.Current(ctx, log, proc.Claude, "s1")
			if ok {
				t.Fatal("an ended session lives on")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.Get(proc.Claude, "s1"); found || err != nil {
			t.Fatalf("the ended record stays: %v", err)
		}
		all := eventsOf(t, log)
		return rec, stamped(all[len(all)-1])
	}
	for _, ev := range []eventlog.Event{{Event: eventlog.End, LastHookAt: contact}, {Event: eventlog.Exit}} {
		rec, whole := end(t, false, ev)
		_, repaired := end(t, true, ev)
		if whole.Event != ev.Event || whole.Cwd != rec.Cwd || !whole.LastHookAt.Equal(ev.LastHookAt) || !reflect.DeepEqual(repaired, whole) {
			t.Fatalf("%s logged %+v, repaired %+v", ev.Event, whole, repaired)
		}
	}

	// A hook's own cwd ends the session; one too large to record is refused,
	// and the session stays as it was until it ends otherwise.
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	rec := full(t, s, log)
	before, head := fileOf(t, recordPath(s)), headOf(t, log)
	limited(t, s.End(ctx, log, rec, eventlog.Event{Event: eventlog.End, Cwd: strings.Repeat("<", MaxRecordBytes/6+1)}), partEnded, MaxRecordBytes)
	if fileOf(t, recordPath(s)) != before || headOf(t, log) != head {
		t.Fatal("a refused end was published")
	}
	if err := s.End(ctx, log, rec, eventlog.Event{Event: eventlog.End, Cwd: "/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if all := eventsOf(t, log); all[len(all)-1].Cwd != "/elsewhere" {
		t.Fatalf("end event %+v", all[len(all)-1])
	}

	// An older tmag's ended record holds the whole session; its end is
	// finished from it.
	old := Record{Agent: proc.Codex, ID: "old", Main: adapter.Idle, Cwd: "/old", Since: t0, LastHookAt: t0, Ended: eventlog.Exit}
	if err := s.Put(old); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := s.Get(proc.Codex, "old")
	if _, ok, err := s.Settle(ctx, log, stored); ok || err != nil {
		t.Fatalf("legacy end: %v %v", ok, err)
	}
	if all := eventsOf(t, log); all[len(all)-1].Event != eventlog.Exit || all[len(all)-1].Cwd != "/old" || all[len(all)-1].Session != "old" {
		t.Fatalf("legacy end event %+v", all[len(all)-1])
	}
}

// A marked record an older tmag left, readable but too large to repair, is
// unrepairable metadata: kept as it is, with nothing logged for it, however
// often it is read.
func TestSettleLeavesAnUnrepairableLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	ctx := context.Background()
	if err := s.Put(Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle}); err != nil {
		t.Fatal(err)
	}
	legacy := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Since: t0, Change: "Stop",
		Cwd: "/" + strings.Repeat("c", MaxRecordBytes/2+1024)}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := recordPath(s)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	head := headOf(t, log)
	for range 2 {
		err := s.Lock(ctx, proc.Claude, "s1", func() error {
			_, _, err := s.Current(ctx, log, proc.Claude, "s1")
			return err
		})
		var fault *RecordError
		if !errors.As(err, &fault) || fault.Category != "repair_exceeds_limit" || fault.ID != "s1" {
			t.Fatalf("Current = %v", err)
		}
		if fileOf(t, path) != string(data) || headOf(t, log) != head {
			t.Fatal("an unrepairable record was changed or logged")
		}
	}
}

// A canceled change returns at once, before encoding even a record too large
// to publish, and publishes nothing.
func TestCanceledChangesReturnBeforeEncoding(t *testing.T) {
	dir := t.TempDir()
	s, log := Store{Dir: dir}, eventlog.Open(dir)
	rec := Record{Agent: proc.Claude, ID: "s1", Main: adapter.Idle, Cwd: strings.Repeat("<", MaxRecordBytes/6+1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	halt := eventlog.Event{Event: eventlog.Halt}
	for name, err := range map[string]error{
		"put":    s.PutContext(ctx, rec),
		"save":   func() error { _, err := s.Save(ctx, log, rec, "Stop"); return err }(),
		"report": func() error { _, err := s.Report(ctx, log, rec, &halt); return err }(),
		"end":    s.End(ctx, log, rec, eventlog.Event{Event: eventlog.Exit}),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a canceled change published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a canceled change logged: %v", err)
	}
}
