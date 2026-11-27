package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// A list marks the caller's session, and for every other one the agent
// session that started it and how it is tied to the caller; a row with
// nothing to say prints as it always has.
func TestListMarksTies(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	me := eventlog.Actor{Agent: "claude", Session: "aaaa0000-1"}
	other := eventlog.Actor{Agent: "claude", Session: "ffff0000-1"}
	v := query.View{Sessions: []query.Session{
		{Agent: "claude", ID: "aaaa0000-1", State: "working", Owes: []eventlog.Obligation{{To: eventlog.Actor{Agent: "codex", Session: "dddd0000-1"}, Kind: eventlog.OweReply}}},
		{Agent: "pi", ID: "bbbb0000-1", State: "working", StartedBy: &me, Owes: []eventlog.Obligation{{To: me, Kind: eventlog.OweReply}}},
		{Agent: "pi", ID: "cccc0000-1", State: "idle", StartedBy: &other},
		{Agent: "codex", ID: "dddd0000-1", State: "idle"},
		{Agent: "claude", ID: "eeee0000-1", State: "idle"},
	}}
	rows := func(c query.Caller) map[string]string {
		var out bytes.Buffer
		(&app{stdout: &out}).printView(v, c, idNames(nil))
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
			f := strings.Fields(line)
			got[f[6]] = strings.Join(f[7:], " ")
		}
		return got
	}
	for id, want := range map[string]string{
		"aaaa0000": "(you)",
		"bbbb0000": "(started by you, owes you)",
		"cccc0000": "(started by ffff0000)",
		"dddd0000": "(you owe it)",
		"eeee0000": "",
	} {
		if got := rows(query.Caller{By: &me})[id]; got != want {
			t.Errorf("an agent's row %s says %q, want %q", id, got, want)
		}
	}
	for id, want := range map[string]string{
		"aaaa0000": "", "bbbb0000": "(started by aaaa0000)", "cccc0000": "(started by ffff0000)", "dddd0000": "", "eeee0000": "",
	} {
		if got := rows(query.Caller{})[id]; got != want {
			t.Errorf("a person's row %s says %q, want %q", id, got, want)
		}
	}
}

// Ids that share their first 8 characters, as Codex's do when started within
// a minute, are listed by as many as tell them apart among the sessions
// named, here and in marks.
func TestListCutsIdsWhereTheyDiffer(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	a, b := "01a218ba-bdcd-7e83-861b-602aeb7d5f1e", "01a218ba-c97f-7830-8126-10d0a894cf82"
	hub := eventlog.Actor{Agent: "codex", Session: a}
	v := query.View{Sessions: []query.Session{
		{Agent: "codex", ID: a, State: "idle"},
		{Agent: "codex", ID: b, State: "idle", StartedBy: &hub},
		{Agent: "claude", ID: "1787a44e-0000-7000-8000-000000000001", State: "idle"},
	}}
	var ids []string
	for _, s := range v.Sessions {
		ids = append(ids, s.ID)
	}
	var out bytes.Buffer
	(&app{stdout: &out}).printView(v, query.Caller{}, idNames(ids))
	for _, want := range []string{" 01a218ba-b ", " 01a218ba-c (started by 01a218ba-b)", " 1787a44e "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list does not hold %q:\n%s", want, out.String())
		}
	}
	if got := idNames([]string{"abc", "abd"})("abc"); got != "abc" {
		t.Errorf("a short id is cut to %q", got)
	}
}
