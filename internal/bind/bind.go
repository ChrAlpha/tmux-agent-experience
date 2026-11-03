package bind

import (
	"path/filepath"
	"regexp"
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
	// is exactly the thread's name, which no other thread shares.
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
	// Threads is required for Codex title evidence; nil disables it.
	Threads Threads
}

// Result is the chosen pane and its primary agent process, or Evidence == Unbound.
type Result struct {
	Pane     tmux.Pane
	AgentPID int
	Evidence string
}

// Resolve applies the evidence rules in order of strength.
func Resolve(w *World, r Request) Result {
	found := func(p tmux.Pane, evidence string) Result {
		agent, _, _ := w.Proc.FindAgent(p.PID)
		return Result{Pane: p, AgentPID: agent.PID, Evidence: evidence}
	}
	if p, ok := byProcess(w, r.Agent, r.PID); ok {
		return found(p, Process)
	}
	codex := r.Agent == proc.Codex && r.Threads != nil
	if codex {
		if p, ok := byTitleID(w, r.SessionID); ok {
			return found(p, TitleID)
		}
	}
	if p, ok := bySession(w, r); ok {
		return found(p, Session)
	}
	if codex {
		if p, ok := byTitleName(w, r); ok {
			return found(p, TitleName)
		}
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
func byProcess(w *World, agent string, pid int) (tmux.Pane, bool) {
	pane, hookAgent, ok := enclosing(w, pid)
	if !ok || hookAgent == nil {
		return tmux.Pane{}, false
	}
	primary, kind, found := w.Proc.FindAgent(pane.PID)
	if !found || kind != agent || primary.PID != hookAgent.PID {
		return tmux.Pane{}, false
	}
	return pane, true
}

func single(panes []tmux.Pane) (tmux.Pane, bool) {
	if len(panes) == 1 {
		return panes[0], true
	}
	return tmux.Pane{}, false
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

func showsID(title, sessionID string) bool {
	if len(sessionID) < codexTitleIDLen {
		return false
	}
	prefix := sessionID[:codexTitleIDLen]
	for _, seg := range TitleSegments(title) {
		if strings.HasPrefix(seg, prefix) {
			return true
		}
	}
	return false
}

func showsName(title, name string) bool {
	if len(name) < minTitleNameLen {
		return false
	}
	for _, seg := range TitleSegments(title) {
		if seg == name {
			return true
		}
	}
	return false
}

// showsOtherThread reports whether a Codex title positively identifies a
// thread other than sessionID: another thread id, or another thread's unique name.
func showsOtherThread(title, sessionID string, threads Threads) bool {
	if showsID(title, sessionID) {
		return false
	}
	if name, _ := threads.Name(sessionID); name != "" && showsName(title, name) {
		return false
	}
	for _, seg := range TitleSegments(title) {
		if uuidPrefixRE.MatchString(seg) {
			return true
		}
		if len(seg) >= minTitleNameLen {
			if id, unique := threads.ByName(seg); unique && id != sessionID {
				return true
			}
		}
	}
	return false
}

func byTitleID(w *World, sessionID string) (tmux.Pane, bool) {
	var matches []tmux.Pane
	for _, p := range w.Panes {
		if w.AgentIn(p) == proc.Codex && showsID(p.Title, sessionID) {
			matches = append(matches, p)
		}
	}
	return single(matches)
}

func bySession(w *World, r Request) (tmux.Pane, bool) {
	var matches []tmux.Pane
	for _, p := range w.Panes {
		rec, ok := w.Record(p)
		if !ok || rec.Session != r.SessionID || rec.Agent != r.Agent {
			continue
		}
		if r.Agent == proc.Codex && r.Threads != nil && showsOtherThread(p.Title, r.SessionID, r.Threads) {
			continue // the TUI switched threads; the record is stale
		}
		matches = append(matches, p)
	}
	return single(matches)
}

func byTitleName(w *World, r Request) (tmux.Pane, bool) {
	name, unique := r.Threads.Name(r.SessionID)
	if !unique {
		return tmux.Pane{}, false
	}
	var matches []tmux.Pane
	for _, p := range w.Panes {
		if w.AgentIn(p) == proc.Codex && samePath(p.Path, r.Cwd) && showsName(p.Title, name) {
			matches = append(matches, p)
		}
	}
	return single(matches)
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

// Record returns the pane's record if it belongs to the agent process that
// currently runs in the pane. Records of exited or replaced agents are stale.
func (w *World) Record(p tmux.Pane) (registry.Record, bool) {
	rec := registry.FromPane(p)
	if !rec.Bound() {
		return rec, false
	}
	agent, kind, ok := w.Proc.FindAgent(p.PID)
	if !ok || kind != rec.Agent || agent.PID != rec.PID {
		return rec, false
	}
	return rec, true
}
