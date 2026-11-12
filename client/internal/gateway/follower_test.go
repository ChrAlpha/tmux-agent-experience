package gateway

import (
	"context"
	"testing"
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
