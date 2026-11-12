package gateway

import (
	"context"
	"testing"
	"time"
)

func TestOverflowingStreamIsCancelled(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	sub := h.subscribe("device", cancel)
	for range cap(sub.frames) {
		h.publish("event: doc\ndata: {}\n\n")
	}
	h.publish("event: doc\ndata: one too many\n\n")
	if ctx.Err() == nil {
		t.Fatal("an overflowing stream stayed open")
	}
	h.mu.Lock()
	_, still := h.subs[sub.id]
	h.mu.Unlock()
	if still {
		t.Fatal("an overflowing stream stayed subscribed")
	}
}

func TestCancelledStreamWritesNoBufferedFrame(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	sub := h.subscribe("device", cancel)
	h.publish("event: doc\ndata: before revocation\n\n")
	h.closeInactive(map[string]bool{})
	if f, ok := next(ctx, sub.frames, nil); ok {
		t.Fatalf("a cancelled stream wrote %q", f)
	}
}

func TestSeenEventsFollowTheSession(t *testing.T) {
	m := newMachineState("here")
	m.apply(Event{Seq: 1, Time: time.Now(), Agent: "claude", Session: "s1", Event: "Stop", State: "idle", Turn: "t1", Seen: "t1"})
	if s, ok := m.session("claude", "s1"); !ok || s.Seen != "t1" {
		t.Fatalf("a state event's seen turn: %+v", s)
	}
	// A Seen event moves the seen turn and changes nothing else.
	m.apply(Event{Seq: 2, Time: time.Now(), Agent: "claude", Session: "s1", Event: "Seen", Turn: "t2"})
	s, ok := m.session("claude", "s1")
	if !ok || s.Seen != "t2" || s.State != "idle" || s.Turn != "t1" {
		t.Fatalf("after a Seen event: %+v", s)
	}
	if seq, _, _, _, _ := m.snapshot(); seq != 2 {
		t.Fatalf("seq = %d", seq)
	}
	// A Seen event for a session the gateway does not show creates nothing.
	m.apply(Event{Seq: 3, Agent: "claude", Session: "gone", Event: "Seen", Turn: "t9"})
	if _, ok := m.session("claude", "gone"); ok {
		t.Fatal("a Seen event made a session")
	}
}
