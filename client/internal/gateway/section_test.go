package gateway

import (
	"testing"
	"time"
)

func TestSections(t *testing.T) {
	now := time.Now()
	idle := func(turn, seen string) Session {
		return Session{State: "idle", Turn: turn, Seen: seen, Since: &now}
	}
	for name, c := range map[string]struct {
		s    Session
		want string
	}{
		"blocked":               {Session{State: "blocked"}, SectionInbox},
		"idle unseen":           {idle("t1", ""), SectionInbox},
		"idle seen":             {idle("t1", "t1"), SectionSettled},
		"idle seen earlier":     {idle("t2", "t1"), SectionInbox},
		"idle without a turn":   {idle("", ""), SectionSettled},
		"working":               {Session{State: "working"}, SectionWorking},
		"no state":              {Session{}, SectionUnknown},
		"blocked although seen": {Session{State: "blocked", Seen: "t1"}, SectionInbox},
	} {
		if got := Section(c.s); got != c.want {
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
	if got := waiting(idle("t1", "")); !got.Equal(now) {
		t.Errorf("idle waiting = %v", got)
	}
}
