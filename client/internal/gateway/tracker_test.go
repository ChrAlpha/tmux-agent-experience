package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSections(t *testing.T) {
	now := time.Now()
	idle := func(turn string) Session { return Session{State: "idle", Turn: turn, Since: &now} }
	for name, c := range map[string]struct {
		s    Session
		seen bool
		want string
	}{
		"blocked":               {Session{State: "blocked"}, false, SectionInbox},
		"idle unseen":           {idle("t1"), false, SectionInbox},
		"idle seen":             {idle("t1"), true, SectionSettled},
		"idle without a turn":   {idle(""), false, SectionSettled},
		"working":               {Session{State: "working"}, false, SectionWorking},
		"no state":              {Session{}, false, SectionUnknown},
		"blocked although seen": {Session{State: "blocked"}, true, SectionInbox},
	} {
		if got := section(c.s, c.seen); got != c.want {
			t.Errorf("%s: %s", name, got)
		}
	}
	// Longest waiting first: a blocked session's oldest request, otherwise
	// its state's age.
	old := now.Add(-time.Hour)
	s := Session{State: "blocked", Since: &now, Requests: []Request{{Since: now}, {Since: old}}}
	if got := waiting(s); !got.Equal(old) {
		t.Errorf("blocked waiting = %v, want %v", got, old)
	}
	if got := waiting(idle("t1")); !got.Equal(now) {
		t.Errorf("idle waiting = %v", got)
	}
}

func TestSeenPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.mark("box", "claude", "s1", "t1")
	second, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !second.isSeen("box", Session{Agent: "claude", ID: "s1", Turn: "t1"}) {
		t.Fatal("a seen turn did not survive a restart")
	}
	// A touch alone is saved on shutdown.
	second.touch("box", []Session{{Agent: "claude", ID: "s1"}})
	second.flush()
	third, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := third.snapshot(); got["box/claude/s1"] != "t1" {
		t.Fatalf("after a touch and restart: %v", got)
	}
	if st, err := os.Stat(filepath.Join(dir, seenFile)); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("seen file: %v %v", st, err)
	}
}

func TestSeenBounds(t *testing.T) {
	tr := newTracker()
	at := time.Now()
	tr.setNow(func() time.Time { return at })
	for i := range seenLimit + 10 {
		tr.mark("box", "claude", string(rune('a'+i%26))+string(rune('a'+i/26)), "turn")
	}
	if got := len(tr.snapshot()); got != seenLimit {
		t.Fatalf("remembered %d turns, want %d", got, seenLimit)
	}
	// A session no machine shows for a week leaves a trace no longer.
	tr = newTracker()
	tr.setNow(func() time.Time { return at })
	tr.mark("box", "claude", "s1", "t1")
	at = at.Add(3 * 24 * time.Hour)
	tr.touch("box", []Session{{Agent: "claude", ID: "s1"}})
	at = at.Add(5 * 24 * time.Hour)
	if got := tr.snapshot(); len(got) != 1 {
		t.Fatalf("a touched session was forgotten: %v", got)
	}
	at = at.Add(3 * 24 * time.Hour)
	if got := tr.snapshot(); len(got) != 0 {
		t.Fatalf("a session shown 8 days ago remains: %v", got)
	}
	// A new turn is not seen until it is marked.
	at = time.Now()
	tr = newTracker()
	tr.setNow(func() time.Time { return at })
	tr.mark("box", "claude", "s1", "t1")
	if !tr.isSeen("box", Session{Agent: "claude", ID: "s1", Turn: "t1"}) {
		t.Fatal("the marked turn is not seen")
	}
	if tr.isSeen("box", Session{Agent: "claude", ID: "s1", Turn: "t2"}) {
		t.Fatal("a later turn is seen")
	}
}
