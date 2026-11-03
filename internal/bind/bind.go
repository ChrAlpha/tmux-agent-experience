package bind

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Evidence that binds a session to a pane, strongest first.
const (
	// Process: the hook descends from the pane's own agent process, with no
	// shared agent service or nested agent in between.
	Process = "process"
	// TitleID: the pane runs Codex and a title segment shows the thread id.
	TitleID = "title-id"
	// Session: the pane's live agent already carries this session, and for
	// Codex its title does not show another thread.
	Session = "session"
	// TitleName: the pane runs Codex in the session's cwd and a title segment
	// other than the project name is exactly the thread's name, which no other
	// thread shares.
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

// Threads resolves Codex thread names. Both lookups report uniqueness: a name
// shared by several threads identifies none of them.
type Threads interface {
	Name(id string) (name string, unique bool)
	ByName(name string) (id string, unique bool)
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

// TitleSegments splits a Codex terminal title into its items, dropping a
// leading activity spinner.
func TitleSegments(title string) []string {
	parts := strings.Split(title, codexTitleSep)
	out := make([]string, 0, len(parts))
	for i, s := range parts {
		s = strings.TrimSpace(s)
		if i == 0 {
			if r, size := firstRune(s); size > 0 && isSpinner(r) {
				s = strings.TrimSpace(s[size:])
			}
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// isSpinner matches the Braille and symbol glyphs used as activity frames.
func isSpinner(r rune) bool {
	return (r >= 0x2800 && r <= 0x28ff) || unicode.Is(unicode.So, r) || unicode.Is(unicode.Sm, r)
}

// claim is what a Codex pane title says about one thread.
type claim int

const (
	silent     claim = iota // the title identifies no thread
	showsID                 // a segment shows the thread's id
	showsName               // a segment is the thread's unique name
	showsOther              // a segment identifies another thread
)

// titleClaim reads a Codex pane title for session. Ids outrank names, and a
// segment equal to the project name (the basename of the pane's directory) is
// never read as an id or a name: the default title ends with it.
func (w *World) titleClaim(p tmux.Pane, session string) claim {
	project := filepath.Base(p.Path)
	var segs []string
	for _, seg := range TitleSegments(p.Title) {
		if seg != project {
			segs = append(segs, seg)
		}
	}
	for _, seg := range segs {
		if len(session) >= codexTitleIDLen && strings.HasPrefix(seg, session[:codexTitleIDLen]) {
			return showsID
		}
	}
	for _, seg := range segs {
		if id := leadingID(seg); uuidPrefixRE.MatchString(id) && !strings.HasPrefix(session, id) {
			return showsOther
		}
	}
	if w.Threads == nil {
		return silent
	}
	var names []string
	for _, seg := range segs {
		if len(seg) >= minTitleNameLen {
			names = append(names, seg)
		}
	}
	name, unique := w.Threads.Name(session)
	if unique && slices.Contains(names, name) {
		return showsName
	}
	// Another thread's name proves a switch only while this thread's own name
	// is known: a thread named moments ago shows a name the index may not
	// hold yet, and an older thread may carry the same one.
	if name == "" {
		return silent
	}
	for _, seg := range names {
		if id, unique := w.Threads.ByName(seg); unique && id != session {
			return showsOther
		}
	}
	return silent
}

// leadingID returns the thread-id characters a title segment starts with,
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
