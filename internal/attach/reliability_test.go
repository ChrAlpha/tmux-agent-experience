package attach

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestRuntimeDeathRecheckedUnderLockForBothRoles(t *testing.T) {
	for _, role := range []proc.Role{proc.Terminal, proc.Service} {
		t.Run(string(role), func(t *testing.T) {
			root := t.TempDir()
			env := Env{Log: eventlog.Open(root), Store: session.Store{Dir: root}}
			r := session.Record{Agent: "codex", ID: "thread", Main: "working", Host: session.Host{PID: 123, Start: "old", Role: role}}
			r.Logged = r.Reports()
			if err := env.Store.Put(r); err != nil {
				t.Fatal(err)
			}
			// The stale outer scan saw death. The locked fresh scan sees the same host.
			env.Snapshot = func(context.Context) (*proc.Table, error) {
				return proc.NewTable([]proc.Process{{PID: 123, Start: "old"}}), nil
			}
			if err := env.RepairRecord(context.Background(), &World{Proc: proc.NewTable(nil)}, r); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := env.Store.Get(r.Agent, r.ID); err != nil || !ok {
				t.Fatal("stale proof ended host", err)
			}
			// PID reuse is death of this runtime host, including shared services.
			env.Snapshot = func(context.Context) (*proc.Table, error) {
				return proc.NewTable([]proc.Process{{PID: 123, Start: "new"}}), nil
			}
			for range 2 {
				if err := env.RepairRecord(context.Background(), &World{Proc: proc.NewTable(nil)}, r); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, err := env.Store.Get(r.Agent, r.ID); err != nil || ok {
				t.Fatal("dead host retained", err)
			}
			events, _, err := env.Log.Since(context.Background(), 0)
			if err != nil || len(events) != 1 || events[0].Event != eventlog.Exit {
				t.Fatalf("events=%v err=%v", events, err)
			}
		})
	}
}

func TestForeignAndIncompleteHostEvidencePreserved(t *testing.T) {
	for _, tc := range []struct{ name, start, realm string }{{"foreign", "known", "different-realm"}, {"missing_start", "", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			env := Env{Log: eventlog.Open(root), Store: session.Store{Dir: root}, Snapshot: func(context.Context) (*proc.Table, error) { return proc.NewTable(nil), nil }}
			r := session.Record{Agent: "codex", ID: "thread", Main: "idle", Host: session.Host{PID: 123, Start: tc.start, Role: proc.Service}}
			if tc.realm != "" {
				r.Observe(time.Now(), tc.realm, true)
			}
			r.Logged = r.Reports()
			if err := env.Store.Put(r); err != nil {
				t.Fatal(err)
			}
			if err := env.RepairRecord(context.Background(), &World{Proc: proc.NewTable(nil)}, r); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := env.Store.Get(r.Agent, r.ID); err != nil || !ok {
				t.Fatalf("uncertain host removed: %v", err)
			}
		})
	}
}

func TestHealthyReconciliationDoesNotRescanProcessesOrExpireState(t *testing.T) {
	root := t.TempDir()
	scans := 0
	env := Env{Log: eventlog.Open(root), Store: session.Store{Dir: root}, Snapshot: func(context.Context) (*proc.Table, error) { scans++; return nil, fmt.Errorf("unexpected full scan") }}
	host := session.Host{PID: 123, Start: "known", Role: proc.Service}
	w := &World{Proc: proc.NewTable([]proc.Process{{PID: host.PID, Start: host.Start}})}
	old := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	var originals []session.Record
	for i := range 40 {
		main := "idle"
		if i%3 == 1 {
			main = "working"
		}
		r := session.Record{Agent: "codex", ID: fmt.Sprintf("thread-%d", i), Host: host, Main: main, Since: old}
		r.Observe(old, "", true)
		if i%3 == 2 {
			prompt := adapter.Prompt{Tool: "Bash"}
			r.Pending = []adapter.Prompt{prompt}
			r.Requests = []session.Request{{ID: fmt.Sprintf("request-%d", i), Prompt: prompt, Since: old}}
		}
		r.Logged = r.Reports()
		if err := env.Store.Put(r); err != nil {
			t.Fatal(err)
		}
		originals = append(originals, r)
	}
	report := Reconcile(context.Background(), env, w)
	if scans != 0 || report.FaultCount != 0 {
		t.Fatalf("healthy pass scans=%d report=%+v", scans, report)
	}
	for _, want := range originals {
		got, found, err := env.Store.Get(want.Agent, want.ID)
		if err != nil || !found || !reflect.DeepEqual(got, want) {
			t.Fatalf("old %s record/request/Since changed: got=%+v want=%+v err=%v", want.State(), got, want, err)
		}
		if label := session.ObservationLabel(got.Observation, time.Now(), 15*time.Minute); label != "quiet" {
			t.Fatalf("age=%s", label)
		}
	}
	// Prospective death invokes exactly one fresh locked scan, whose evidence
	// can contradict the outer scan and preserve the same authoritative record.
	env.Snapshot = func(context.Context) (*proc.Table, error) { scans++; return w.Proc, nil }
	if err := env.RepairRecord(context.Background(), &World{Proc: proc.NewTable(nil)}, originals[0]); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("suspected death fresh scans=%d", scans)
	}
	if got, found, err := env.Store.Get(originals[0].Agent, originals[0].ID); err != nil || !found || !reflect.DeepEqual(got, originals[0]) {
		t.Fatalf("stale death scan changed state: %+v %v", got, err)
	}
}
