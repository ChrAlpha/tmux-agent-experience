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
	first.configure([]string{"box"})
	first.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	first.mark("box", "claude", "s1", "t1")
	second, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.configure([]string{"box"})
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
	tr.configure([]string{"box"})
	present := make([]Session, 0, seenLimit+10)
	for i := range seenLimit + 10 {
		present = append(present, Session{Agent: "claude", ID: string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	tr.observe("box", present)
	for i := range seenLimit + 10 {
		tr.mark("box", "claude", string(rune('a'+i%26))+string(rune('a'+i/26)), "turn")
	}
	if got := len(tr.snapshot()); got != seenLimit {
		t.Fatalf("remembered %d turns, want %d", got, seenLimit)
	}
	// A present session stays however quiet it is.
	tr = newTracker()
	tr.setNow(func() time.Time { return at })
	tr.configure([]string{"box"})
	tr.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	tr.mark("box", "claude", "s1", "t1")
	at = at.Add(8 * 24 * time.Hour)
	if got := tr.snapshot(); len(got) != 1 {
		t.Fatalf("a present session was forgotten: %v", got)
	}
	// When it leaves, absence starts there.
	tr.leave("box", []Session{{Agent: "claude", ID: "s1"}})
	at = at.Add(6 * 24 * time.Hour)
	if got := tr.snapshot(); len(got) != 1 {
		t.Fatalf("an absent session was forgotten early: %v", got)
	}
	at = at.Add(24*time.Hour + time.Hour)
	if got := tr.snapshot(); len(got) != 0 {
		t.Fatalf("a session absent for a week remains: %v", got)
	}
	// A new turn is not seen until it is marked.
	at = time.Now()
	tr = newTracker()
	tr.setNow(func() time.Time { return at })
	tr.configure([]string{"box"})
	tr.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	tr.mark("box", "claude", "s1", "t1")
	if !tr.isSeen("box", Session{Agent: "claude", ID: "s1", Turn: "t1"}) {
		t.Fatal("the marked turn is not seen")
	}
	if tr.isSeen("box", Session{Agent: "claude", ID: "s1", Turn: "t2"}) {
		t.Fatal("a later turn is seen")
	}
}

func TestSeenPresenceSurvivesAnotherMachinesPrune(t *testing.T) {
	tr := newTracker()
	at := time.Now()
	tr.setNow(func() time.Time { return at })
	tr.configure([]string{"a", "b"})
	tr.observe("a", []Session{{Agent: "claude", ID: "s1"}})
	tr.mark("a", "claude", "s1", "t1")
	at = at.Add(8 * 24 * time.Hour)
	// Another machine's follower prunes before a records the removal.
	tr.touch("b", nil)
	if got := tr.snapshot(); got["a/claude/s1"] != "t1" {
		t.Fatalf("a present marker was pruned by another machine: %v", got)
	}
	tr.leave("a", []Session{{Agent: "claude", ID: "s1"}})
	if got := tr.snapshot(); got["a/claude/s1"] != "t1" {
		t.Fatalf("a marker was lost when it left: %v", got)
	}
	at = at.Add(7*24*time.Hour + time.Hour)
	if got := tr.snapshot(); len(got) != 0 {
		t.Fatalf("an absent marker remained: %v", got)
	}
}

func TestSeenFirstSnapshotMarksMissingSessionsAbsent(t *testing.T) {
	dir := t.TempDir()
	first, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.configure([]string{"box"})
	first.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	first.mark("box", "claude", "s1", "t1")
	second, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.configure([]string{"box"})
	at := time.Now()
	second.setNow(func() time.Time { return at })
	// The gateway restarts and the machine's first snapshot lacks the session.
	second.observe("box", nil)
	if got := second.snapshot(); got["box/claude/s1"] != "t1" {
		t.Fatalf("a marker was lost on the first snapshot: %v", got)
	}
	second.flush()
	third, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	at = at.Add(7*24*time.Hour + time.Hour)
	third.setNow(func() time.Time { return at })
	if got := third.snapshot(); len(got) != 0 {
		t.Fatalf("a marker absent since the first snapshot remained: %v", got)
	}
}

func TestMarkAfterLeaveRecordsNothing(t *testing.T) {
	tr := newTracker()
	at := time.Now()
	tr.setNow(func() time.Time { return at })
	tr.configure([]string{"box"})
	tr.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	// The ending event wins the race with the page's mark.
	tr.leave("box", []Session{{Agent: "claude", ID: "s1"}})
	if got := tr.mark("box", "claude", "s1", "t1"); got != "" {
		t.Fatalf("marked a session that had left: %q", got)
	}
	if got := tr.snapshot(); len(got) != 0 {
		t.Fatalf("a marker was created after the session left: %v", got)
	}
}

func TestSeenAbsenceSurvivesAThrottledSave(t *testing.T) {
	dir := t.TempDir()
	first, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	first.setNow(func() time.Time { return at })
	first.configure([]string{"box"})
	first.observe("box", []Session{{Agent: "claude", ID: "s1"}})
	first.mark("box", "claude", "s1", "t1")
	at = at.Add(30 * time.Second)
	first.observe("box", nil) // absent right after the mark's save
	// A crash: reload only what reached disk, without flushing memory.
	second, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	after := at.Add(7*24*time.Hour + time.Hour)
	second.setNow(func() time.Time { return after })
	second.configure([]string{"box"})
	second.observe("box", nil)
	if got := second.snapshot(); len(got) != 0 {
		t.Fatalf("an absence lost by the save throttle remained: %v", got)
	}
}

func TestSeenRestartWithoutAMachineExpires(t *testing.T) {
	dir := t.TempDir()
	first, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	first.setNow(func() time.Time { return at })
	first.configure([]string{"here", "gone"})
	first.observe("here", []Session{{Agent: "claude", ID: "s1"}})
	first.observe("gone", []Session{{Agent: "claude", ID: "s2"}})
	first.mark("here", "claude", "s1", "t1")
	first.mark("gone", "claude", "s2", "t2")

	second, err := loadTracker(dir)
	if err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Hour)
	second.setNow(func() time.Time { return at })
	second.configure([]string{"here"})
	second.observe("here", []Session{{Agent: "claude", ID: "s1"}})
	at = at.Add(7*24*time.Hour + time.Hour)
	second.observe("here", []Session{{Agent: "claude", ID: "s1"}})
	got := second.snapshot()
	if got["here/claude/s1"] != "t1" {
		t.Fatalf("the configured machine's marker was lost: %v", got)
	}
	if _, ok := got["gone/claude/s2"]; ok {
		t.Fatalf("a marker of an unconfigured machine remained: %v", got)
	}
}
