package bind

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/codexidx"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Evidence that binds a session to a pane, strongest first.
const (
	// Process: the hook descends from the pane's own agent process, with no
	// shared agent service or nested agent in between.
	Process = "process"
	// TitleID: the pane runs Codex and a title item shows the thread id.
	TitleID = "title-id"
	// Session: the pane's live agent already carries this session, and for
	// Codex its title does not show another thread.
	Session = "session"
	// TitleName: the pane runs Codex in the session's cwd and a title item
	// other than the project shows the thread's name as Codex displays it,
	// which no other thread's name displays as.
	TitleName = "title-name"
	// Unbound: no evidence, or evidence pointing at more than one pane.
	Unbound = "unbound"
)

// codexTitleIDLen is how many leading characters of a thread id the Codex TUI
// shows in its title before truncating with "...".
const codexTitleIDLen = 29

// minTitleNameLen avoids treating trivially short thread names as evidence.
const minTitleNameLen = 4

// codexTitleSep joins the items of the Codex terminal title.
const codexTitleSep = " | "

var uuidPrefixRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-`)

// Threads resolves Codex thread names.
type Threads interface {
	// Name returns the thread's name, or "" when it has none.
	Name(id string) string
	// Showing returns the threads whose name the Codex title displays as
	// shown (see codexidx.Display); one name may stand for several threads.
	Showing(shown string) []string
}

// Request describes the session to place.
type Request struct {
	Agent     string
	SessionID string
	Cwd       string
	// PID is the process to start the ancestry walk from (the hook itself).
	PID int
}

// Result is the chosen pane and its primary agent process, or Evidence == Unbound.
type Result struct {
	Pane     tmux.Pane
	AgentPID int
	Evidence string
}

// Resolve applies the evidence rules in order of strength. The first rule
// that matches decides: one pane binds, several panes leave the session
// unbound rather than letting weaker evidence pick one of them.
func Resolve(w *World, r Request) Result {
	rules := []struct {
		evidence string
		match    func() []tmux.Pane
	}{
		{Process, func() []tmux.Pane { return byProcess(w, r.Agent, r.PID) }},
		{TitleID, func() []tmux.Pane { return byTitle(w, r, showsID) }},
		{Session, func() []tmux.Pane { return bySession(w, r) }},
		{TitleName, func() []tmux.Pane { return byTitle(w, r, showsName) }},
	}
	for _, rule := range rules {
		panes := rule.match()
		if len(panes) == 0 {
			continue
		}
		if len(panes) > 1 {
			return Result{Evidence: Unbound}
		}
		agent, _, _ := w.Proc.FindAgent(panes[0].PID)
		return Result{Pane: panes[0], AgentPID: agent.PID, Evidence: rule.evidence}
	}
	return Result{Evidence: Unbound}
}

// Enclosing returns the pane whose process tree contains pid, stopping at shared
// agent services: a process run by a daemon belongs to no pane, whatever
// environment or parentage the daemon inherited.
func Enclosing(w *World, pid int) (tmux.Pane, bool) {
	p, _, ok := enclosing(w, pid)
	return p, ok
}

// enclosing also returns the nearest agent process on the way up, if any.
func enclosing(w *World, pid int) (tmux.Pane, *proc.Process, bool) {
	byPID := make(map[int]tmux.Pane, len(w.Panes))
	for _, p := range w.Panes {
		byPID[p.PID] = p
	}
	var agent *proc.Process
	for _, a := range w.Proc.Ancestors(pid) {
		p, known := w.Proc.Get(a)
		if known && proc.IsCodexService(p) {
			return tmux.Pane{}, nil, false
		}
		// The pane's own process may be the agent (tmux new-window claude).
		if known && agent == nil && proc.AgentKind(p) != "" {
			agent = &p
		}
		if pane, ok := byPID[a]; ok {
			return pane, agent, true
		}
	}
	return tmux.Pane{}, nil, false
}

// byProcess binds when the hook's nearest agent ancestor is the pane's own
// agent. A hook from an agent nested inside another agent's pane (say, codex
// exec run by Claude's Bash tool) proves nothing about that pane.
func byProcess(w *World, agent string, pid int) []tmux.Pane {
	pane, hookAgent, ok := enclosing(w, pid)
	if !ok || hookAgent == nil {
		return nil
	}
	primary, kind, found := w.Proc.FindAgent(pane.PID)
	if !found || kind != agent || primary.PID != hookAgent.PID {
		return nil
	}
	return []tmux.Pane{pane}
}

// byTitle matches Codex panes whose title shows the thread the given way. A
// name only counts in the session's cwd, where the TUI runs.
func byTitle(w *World, r Request, want claim) []tmux.Pane {
	if r.Agent != proc.Codex {
		return nil
	}
	var matches []tmux.Pane
	for _, p := range w.Panes {
		if w.AgentIn(p) != proc.Codex || w.titleClaim(p, r.SessionID) != want {
			continue
		}
		if want == showsName && !samePath(p.Path, r.Cwd) {
			continue
		}
		matches = append(matches, p)
	}
	return matches
}

func bySession(w *World, r Request) []tmux.Pane {
	var matches []tmux.Pane
	for _, p := range w.Panes {
		if rec, ok := w.Record(p); ok && rec.Session == r.SessionID && rec.Agent == r.Agent {
			matches = append(matches, p)
		}
	}
	return matches
}

// codexTitle is a Codex 0.166 terminal title in the layouts tmag reads: the
// default items (activity, thread-name, project-name), optionally with
// thread-id first. Items are joined by " | ", the activity glyphs by spaces,
// and the project label always comes last. Its value depends on the thread's
// repository root and truncation, so it is located by position, never by
// guessing the label from a directory name.
type codexTitle struct {
	items   []string // every item but the project, in order
	project string
}

// readTitle splits a title into its items, dropping activity glyphs and the
// action-required prefix the TUI shows while it waits for the user.
func readTitle(title string) codexTitle {
	var parts []string
	for i, item := range strings.Split(title, codexTitleSep) {
		for _, part := range splitActivity(item) {
			if i == 0 && len(parts) == 0 && actionRequired[part] {
				continue
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return codexTitle{}
	}
	return codexTitle{items: parts[:len(parts)-1], project: parts[len(parts)-1]}
}

// actionRequired holds both frames of the prefix Codex blinks in the title
// while an approval waits.
var actionRequired = map[string]bool{"[ ! ] Action Required": true, "[ . ] Action Required": true}

// splitActivity cuts an item at activity glyphs standing alone between spaces,
// where the TUI joins its activity indicator to the items around it.
func splitActivity(item string) []string {
	var out, words []string
	flush := func() {
		if len(words) > 0 {
			out = append(out, strings.Join(words, " "))
			words = nil
		}
	}
	for _, word := range strings.Split(item, " ") {
		if word != "" && strings.IndexFunc(word, func(r rune) bool { return !isActivity(r) }) < 0 {
			flush()
			continue
		}
		words = append(words, word)
	}
	flush()
	trimmed := out[:0]
	for _, s := range out {
		if s = strings.TrimSpace(s); s != "" {
			trimmed = append(trimmed, s)
		}
	}
	return trimmed
}

// isActivity matches the title's activity glyphs: Braille spinner frames and
// the dictation dot.
func isActivity(r rune) bool { return (r >= 0x2800 && r <= 0x28ff) || r == '●' }

// shownNames returns the item runs that may be a thread name: single items
// and, for names that contain the separator, adjacent items joined by it.
func (t codexTitle) shownNames() []string {
	var out []string
	for i := range t.items {
		for j := i + 1; j <= len(t.items); j++ {
			if s := strings.Join(t.items[i:j], codexTitleSep); len(s) >= minTitleNameLen {
				out = append(out, s)
			}
		}
	}
	return out
}

// claim is what a Codex pane title says about one thread.
type claim int

const (
	silent     claim = iota // the title identifies no thread
	showsID                 // an item shows the thread's id
	showsName               // an item is the thread's name, shown by no other thread
	showsOther              // an item identifies another thread
)

// titleClaim reads a Codex pane title for session. Ids outrank names. Only a
// match with this session's own id may come from the project item: a project
// directory can look like any other id, or carry any name.
func (w *World) titleClaim(p tmux.Pane, session string) claim {
	t := readTitle(p.Title)
	if len(session) >= codexTitleIDLen {
		for _, item := range append(slices.Clone(t.items), t.project) {
			if strings.HasPrefix(item, session[:codexTitleIDLen]) {
				return showsID
			}
		}
	}
	for _, item := range t.items {
		if id := leadingID(item); uuidPrefixRE.MatchString(id) && !strings.HasPrefix(session, id) {
			return showsOther
		}
	}
	if w.Threads == nil {
		return silent
	}
	shown := t.shownNames()
	for _, s := range shown {
		if ids := w.Threads.Showing(s); len(ids) == 1 && ids[0] == session {
			return showsName
		}
	}
	// Another thread's name proves a switch only while this thread's own name
	// is known and not on display: a thread named moments ago shows a name the
	// index may not hold yet, and an older thread may carry the same one.
	own := codexidx.Display(w.Threads.Name(session))
	if own == "" || slices.Contains(shown, own) {
		return silent
	}
	for _, s := range shown {
		if len(w.Threads.Showing(s)) > 0 {
			return showsOther
		}
	}
	return silent
}

// leadingID returns the thread-id characters a title item starts with,
// without the "..." of a truncated id or any text after it.
func leadingID(seg string) string {
	end := strings.IndexFunc(seg, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-'
	})
	if end < 0 {
		return seg
	}
	return seg[:end]
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return canonical(a) == canonical(b)
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Record returns the pane's record if it still describes the pane: it belongs
// to the agent process that runs there now and, for Codex, the title does not
// show another thread. Records of exited or replaced agents, or of a thread
// the TUI has switched away from, are stale.
func (w *World) Record(p tmux.Pane) (registry.Record, bool) {
	rec := registry.FromPane(p)
	if !rec.Bound() {
		return rec, false
	}
	agent, kind, ok := w.Proc.FindAgent(p.PID)
	if !ok || kind != rec.Agent || agent.PID != rec.PID {
		return rec, false
	}
	if kind == proc.Codex && w.titleClaim(p, rec.Session) == showsOther {
		return rec, false
	}
	return rec, true
}
