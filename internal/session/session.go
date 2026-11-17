// Package session holds the state of agent sessions, wherever they run.
//
// A session is what an agent's hooks report on: one Claude Code session or
// Codex thread, identified by its agent and id. Its state never depends on a
// terminal; which panes show it is the business of package attach.
package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/driver"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Host is the process a session runs in, as its hooks found it: an
// interactive client, or a background service shared by several sessions.
// The session lives as long as this process does, unless it ends first.
type Host struct {
	PID   int       `json:"pid"`
	Start string    `json:"start,omitempty"`
	Role  proc.Role `json:"role"`
}

// Record is the state of one session.
type Record struct {
	// LastHookAt is accepted hook contact, independent of state age or authority.
	LastHookAt time.Time `json:"last_hook_at,omitzero"`
	Agent      string    `json:"agent"` // one of proc.Agents
	ID         string    `json:"id"`
	Cwd        string    `json:"cwd,omitempty"`
	Host       Host      `json:"host"`
	// Transcript is the path of the agent's transcript of the session, as its
	// hooks report it. The file may not exist before the first turn.
	Transcript string `json:"transcript,omitempty"`
	// Inbox is the socket the session's own agent serves for messages, and
	// the process that serves it, as its hooks proved it.
	Inbox *Inbox `json:"inbox,omitempty"`
	// Shown lists every pane attached to the session, on any server and by
	// any evidence, with the client each attachment is for: an Attach lists a
	// pane before the pane shows the session, and a Detach unlists it after.
	// A hook updates these panes without listing panes or processes.
	Shown []Shown `json:"shown,omitempty"`

	// Main is the main agent's own state (working, idle or unknown) and
	// MainKind qualifies it (error after a failed turn, compacting during one).
	Main     string `json:"main"`
	MainKind string `json:"main_kind,omitempty"`
	// Reason is why the last turn failed, with KindError.
	Reason string `json:"reason,omitempty"`
	// Turn is the main agent's current or last turn.
	Turn string `json:"turn,omitempty"`

	// Permission is the latest permission class the session's hooks reported:
	// bypass or prompting. It is not part of what an event reports.
	Permission string `json:"permission,omitempty"`
	// StatusFile is where Claude Code writes the session's native status, as
	// its hooks found it. tmag reads it to see a turn end without a hook.
	StatusFile string `json:"status_file,omitempty"`

	// Requests are the prompts awaiting the user, oldest first, each under an
	// id tmag answers it by: a prompt two answer hooks wait on has two.
	Requests []Request `json:"requests,omitempty"`
	// Calls are the tool calls started and not yet over.
	Calls []adapter.Call `json:"calls,omitempty"`
	// Done are calls that finished before their start was seen; a late start
	// for one of them is not a running call. Cleared with every Set.
	Done []string `json:"done,omitempty"`
	// Background is the work still running for the session besides the main
	// agent's turn: subagents and, for Claude Code, other background tasks.
	Background []string `json:"background,omitempty"`
	// Subagents are the subagents seen in this session, most recent last,
	// running or not. A Codex TUI may show any of them.
	Subagents []string `json:"subagents,omitempty"`

	// Since is when the state, as State reports it, last changed.
	Since time.Time `json:"since"`
	// Sent is when tmag last submitted a prompt that no turn has confirmed.
	Sent time.Time `json:"sent,omitzero"`
	// Queued is when tmag last sent the session a prompt through its own
	// message channel, which no hook of its main agent has followed:
	// nothing more goes through that channel until the agent reaches its
	// next step.
	Queued time.Time `json:"queued,omitzero"`
	// Halted is when tmag last interrupted the main agent, until the agent
	// reports again: Claude Code reports nothing on an interrupt.
	Halted time.Time `json:"halted,omitzero"`
}

// Inbox is the socket a session's own agent serves for messages, and the
// process that serves it: the one whose environment or extension named the
// socket.
type Inbox struct {
	Path  string `json:"path"`
	PID   int    `json:"pid"`
	Start string `json:"start,omitempty"`
}

// SameInbox reports whether a and b name the same socket and process.
func SameInbox(a, b *Inbox) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Shown is a pane attached to a session, and the client process the
// attachment is for.
type Shown struct {
	Server string `json:"server"`
	Pane   string `json:"pane"`
	PID    int    `json:"pid"`
	Start  string `json:"start,omitempty"`
}

// Bounds of a listed pane, like every field a record keeps: a socket path
// fits in PATH_MAX, a tmux pane id is short, and a process start is a tick
// count or ps's lstart words. A record lists a few panes at most.
const (
	maxShownServer = 1024
	maxShownPane   = 16
	maxShownStart  = 64
	maxShown       = 8
)

// Valid reports whether s names a pane of a server and a client process.
func (s Shown) Valid() bool {
	if s.PID <= 0 || len(s.Server) == 0 || len(s.Server) > maxShownServer || !filepath.IsAbs(s.Server) ||
		len(s.Pane) < 2 || len(s.Pane) > maxShownPane || len(s.Start) > maxShownStart {
		return false
	}
	if s.Pane[0] != '%' {
		return false
	}
	for i := 1; i < len(s.Pane); i++ {
		if s.Pane[i] < '0' || s.Pane[i] > '9' {
			return false
		}
	}
	for i := 0; i < len(s.Server); i++ {
		if c := s.Server[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	for i := 0; i < len(s.Start); i++ {
		switch c := s.Start[i]; {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z',
			c == ' ', c == ':', c == '.', c == '_', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// ShownFor returns the listed pane attached for the client pid, started at
// start.
func (r Record) ShownFor(pid int, start string) (Shown, bool) {
	i := slices.IndexFunc(r.Shown, func(s Shown) bool { return s.PID == pid && s.Start == start })
	if i < 0 {
		return Shown{}, false
	}
	return r.Shown[i], true
}

// Lists reports whether the record lists s.
func (r Record) Lists(s Shown) bool { return slices.Contains(r.Shown, s) }

// List lists s as the attachment of its pane, replacing what the record
// listed for that pane, and reports whether the record changed.
func (r *Record) List(s Shown) bool {
	if r.Lists(s) {
		return false
	}
	r.Unlist(s.Server, s.Pane)
	r.Shown = newest(append(r.Shown, s), maxShown)
	return true
}

// Unlist stops listing the pane, and reports whether the record changed.
func (r *Record) Unlist(server, pane string) bool {
	n := len(r.Shown)
	r.Shown = slices.DeleteFunc(r.Shown, func(s Shown) bool { return s.Server == server && s.Pane == pane })
	if len(r.Shown) == 0 {
		r.Shown = nil
	}
	return len(r.Shown) != n
}

// Request is a pending prompt under the id tmag answers it by.
type Request struct {
	ID     string         `json:"id"`
	Prompt adapter.Prompt `json:"prompt"`
	Turn   string         `json:"turn,omitempty"`
	// Call is the tool call the request asks about, when the last Ask for
	// its prompt can be about one call only: the one running call of the
	// prompt's tool by the agent that asked. An agent service names the call
	// its approval is for, so there a request is answered only with one.
	Call string `json:"call,omitempty"`
	// Hook says the id is an answer hook's, which waits for the answer.
	Hook  bool      `json:"hook,omitempty"`
	Since time.Time `json:"since"`
	// Answered is when tmag last took an answer to the request for an agent
	// service to deliver, which reports it only once it is over.
	Answered time.Time `json:"answered,omitzero"`
}

// NewRequestID returns a fresh request id: short, so that a socket path
// built from it stays within the platform's limit.
func NewRequestID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Request returns the open request with the given id.
func (r Record) Request(id string) (Request, bool) {
	i := slices.IndexFunc(r.Requests, func(q Request) bool { return q.ID == id })
	if i < 0 {
		return Request{}, false
	}
	return r.Requests[i], true
}

// Pending are the prompts awaiting the user, oldest first: those its open
// requests name.
func (r Record) Pending() []adapter.Prompt {
	var out []adapter.Prompt
	for _, q := range r.Requests {
		if !slices.Contains(out, q.Prompt) {
			out = append(out, q.Prompt)
		}
	}
	return out
}

// RequestRefs are the session's requests as events and views report them.
func (r Record) RequestRefs() []eventlog.Request {
	var out []eventlog.Request
	for _, q := range r.Requests {
		out = append(out, eventlog.Request{ID: q.ID, Kind: q.Prompt.RequestKind(), Tool: q.Prompt.ToolName(), Agent: q.Prompt.Agent, Since: q.Since})
	}
	return out
}

// Report is what an event reports of a session, contact time aside. Lists
// are joined, so that reports compare with ==.
type Report struct {
	State      string
	Kind       string
	Reason     string
	Turn       string
	Cwd        string
	Runtime    string
	Transcript string
	Subagents  string
	Requests   string
	Since      time.Time
}

// Reports is what an event of the session reports, contact time aside: when
// it differs from what the session reported before a change, the change is
// logged.
func (r Record) Reports() Report {
	return Report{
		State: r.State(), Kind: r.Kind(), Reason: r.Reason, Turn: r.Turn, Cwd: r.Cwd, Runtime: string(r.Host.Role),
		Transcript: r.Transcript, Subagents: strings.Join(r.RunningSubagents(), "\n"),
		Requests: strings.Join(r.requestIDs(), "\n"), Since: r.Since.UTC(),
	}
}

func (r Record) requestIDs() []string {
	var out []string
	for _, q := range r.Requests {
		out = append(out, q.ID)
	}
	return out
}

// Report fills ev with everything an event reports of the session.
func (r Record) Report(ev *eventlog.Event) {
	ev.Agent, ev.Session = r.Agent, r.ID
	ev.LastHookAt = r.LastHookAt
	ev.State, ev.Kind, ev.Reason, ev.Turn, ev.Cwd = r.State(), r.Kind(), r.Reason, r.Turn, r.Cwd
	ev.Since = nil
	if !r.Since.IsZero() {
		since := r.Since.UTC()
		ev.Since = &since
	}
	ev.Runtime = string(r.Host.Role)
	ev.Transcript = r.TranscriptRef()
	ev.Subagents = r.RunningSubagents()
	ev.Capabilities = r.Capabilities()
	ev.Requests = r.RequestRefs()
}

// Capabilities are the verbs some channel carries to the session. They follow
// from its agent and runtime, which every report includes.
func (r Record) Capabilities() []string { return driver.Capabilities(r.Agent, string(r.Host.Role)) }

// TranscriptRef locates the session's transcript and names its format, or
// returns nil when the hooks did not say where it is.
func (r Record) TranscriptRef() *eventlog.Transcript {
	if r.Transcript == "" {
		return nil
	}
	format := "claude-jsonl"
	switch r.Agent {
	case proc.Codex:
		format = "codex-rollout"
	case proc.Pi, proc.OMP:
		format = "pi-jsonl"
	}
	return &eventlog.Transcript{Path: r.Transcript, Format: format}
}

// RunningSubagents are the session's subagents still running, oldest first.
func (r Record) RunningSubagents() []string {
	var out []string
	for _, s := range r.Subagents {
		if slices.Contains(r.Background, s) {
			out = append(out, s)
		}
	}
	return out
}

// Bounds of what a record keeps, far above what an agent does: hook input is
// bounded where it is read (adapter.Decode), and these bound how much of it
// a session accumulates, so that a record, and an event reporting it, stay
// small whatever an agent sends. The oldest entries go first.
const (
	maxRequests  = 64
	maxCalls     = 64
	maxDone      = 64
	maxTasks     = 64
	maxSubagents = 32
)

// State is the session's state as tmag reports it: blocked while a prompt is
// open, working during a turn or while background work runs, otherwise the
// main agent's state.
func (r Record) State() string {
	state, _ := r.view()
	return state
}

// Kind qualifies State: the kind of prompt while blocked, background while
// only background work runs, error after a failed turn.
func (r Record) Kind() string {
	_, kind := r.view()
	return kind
}

func (r Record) view() (string, string) {
	switch {
	case len(r.Requests) > 0:
		return adapter.Blocked, r.Requests[0].Prompt.Kind()
	case r.Main == adapter.Working:
		return adapter.Working, r.MainKind
	case r.Main == adapter.Idle && len(r.Background) > 0:
		return adapter.Working, adapter.KindBackground
	case r.Main == "":
		return adapter.Unknown, ""
	}
	return r.Main, r.MainKind
}

// ShowsSubagent reports whether id is one of the session's subagents.
func (r Record) ShowsSubagent(id string) bool { return slices.Contains(r.Subagents, id) }

// Apply returns the session after tr, and whether anything changed. cur is
// the zero Record for a session not seen before; now stamps a state change.
//
// Open prompts block the session. A prompt closes only on evidence that the
// user dealt with it: the tool call it may be about is over, the agent's
// calls settled or the agent stopped, or the main agent's turn ended.
func Apply(cur Record, tr adapter.Transition, now time.Time) (Record, bool) {
	rec := cur
	rec.Shown = slices.Clone(cur.Shown)
	rec.Requests = slices.Clone(cur.Requests)
	rec.Calls = slices.Clone(cur.Calls)
	rec.Done = slices.Clone(cur.Done)
	rec.Background = slices.Clone(cur.Background)
	rec.Subagents = slices.Clone(cur.Subagents)
	main := tr.Call.Agent == "" && tr.Prompt.Agent == ""

	switch tr.Action {
	case adapter.Set:
		// Stop and UserPromptSubmit run synchronously, so they arrive in turn
		// order; a late notification about an earlier turn changes nothing.
		if tr.Late && tr.Turn != cur.Turn {
			return cur, false
		}
		// A failed compaction ends one that was marked, and nothing otherwise.
		if tr.OnlyCompacting && cur.MainKind != adapter.KindCompacting {
			return cur, false
		}
		if tr.Fresh {
			rec.Background, rec.Turn = nil, ""
		}
		if tr.Background != nil {
			rec.Background = slices.Clone(*tr.Background)
		}
		keep := func(agent string) bool {
			return agent != "" && (tr.KeepSubagents || slices.Contains(rec.Background, agent))
		}
		rec.Requests = slices.DeleteFunc(rec.Requests, func(q Request) bool { return !keep(q.Prompt.Agent) })
		rec.Calls = slices.DeleteFunc(rec.Calls, func(c adapter.Call) bool { return !keep(c.Agent) })
		rec.Done = nil
		rec.Main, rec.MainKind, rec.Reason = tr.State, tr.Kind, tr.Reason
		if tr.State == adapter.Working {
			rec.Sent = time.Time{} // the turn confirms the prompt tmag sent
		}
	case adapter.Ask:
		// A reminder cannot be told apart from a prompt already pending, and
		// outside a turn it cannot be about one.
		if tr.Reminder && (len(cur.Requests) > 0 || cur.State() != adapter.Working) {
			return cur, false
		}
		if tr.Request == "" {
			tr.Request = NewRequestID()
		}
		rec.Calls = startCall(rec.Calls, rec.Done, tr.Call)
		rec.Requests = request(rec.Requests, tr, asked(rec.Calls, tr), now)
		rec.running(main)
	case adapter.Start:
		rec.Calls = startCall(rec.Calls, rec.Done, tr.Call)
		rec.running(main)
	case adapter.Finish:
		c := tr.Call
		if i := slices.IndexFunc(rec.Calls, func(o adapter.Call) bool { return o.ID == c.ID }); i >= 0 {
			rec.Calls = slices.Delete(rec.Calls, i, i+1)
		} else if c.ID != "" && !slices.Contains(rec.Done, c.ID) {
			rec.Done = append(rec.Done, c.ID)
		}
		running := slices.ContainsFunc(rec.Calls, func(o adapter.Call) bool { return o.Agent == c.Agent && o.Tool == c.Tool })
		if p := (adapter.Prompt{Agent: c.Agent, Tool: c.Tool}); !running && asks(rec.Requests, p) {
			rec.Requests = closePrompt(rec.Requests, p)
		} else {
			rec.Requests = answerUnnamed(rec.Requests)
		}
	case adapter.Settle:
		rec.settle(tr.Call.Agent)
	case adapter.Answer:
		if tr.Request != "" {
			// Decided through tmag: the request closes, and its prompt with
			// it unless another request waits on the same prompt.
			if _, ok := rec.Request(tr.Request); !ok {
				return cur, false
			}
			rec.Requests = closeRequest(rec.Requests, tr.Request)
			break
		}
		if asks(rec.Requests, tr.Prompt) {
			rec.Requests = closePrompt(rec.Requests, tr.Prompt)
		} else {
			rec.Requests = answerUnnamed(rec.Requests)
		}
	case adapter.Spawn:
		agent := tr.Call.Agent
		if !slices.Contains(rec.Background, agent) {
			rec.Background = append(rec.Background, agent)
		}
		rec.Subagents = append(slices.DeleteFunc(rec.Subagents, func(s string) bool { return s == agent }), agent)
	case adapter.Reap:
		agent := tr.Call.Agent
		rec.Background = slices.DeleteFunc(rec.Background, func(s string) bool { return s == agent })
		rec.settle(agent)
	default:
		return cur, false
	}
	if main && tr.Turn != "" {
		rec.Turn = tr.Turn
	}
	if main {
		rec.Halted = time.Time{} // the main agent reports again
		if !tr.Synthetic {
			rec.Queued = time.Time{} // and has reached its next step
		}
	}
	normalize(&rec)
	if rec.State() != cur.State() || rec.Kind() != cur.Kind() || cur.Since.IsZero() {
		rec.Since = now.UTC().Truncate(time.Second)
	}
	return rec, !rec.Same(cur)
}

// request opens the prompt an Ask is about. A prompt no request names gets
// the Ask's id; an answer hook's id names the prompt's request no hook waits
// on, or else adds one: two hooks may wait on equal prompts. Each Ask asks
// anew, so the request it names is about call from now on.
func request(reqs []Request, tr adapter.Transition, call string, now time.Time) []Request {
	if asks(reqs, tr.Prompt) && !tr.Waits {
		for i := range reqs {
			if reqs[i].Prompt == tr.Prompt && !reqs[i].Hook {
				reqs[i].Call = call
			}
		}
		return reqs
	}
	if tr.Waits {
		if i := slices.IndexFunc(reqs, func(q Request) bool { return q.Prompt == tr.Prompt && !q.Hook }); i >= 0 {
			reqs[i].ID, reqs[i].Hook, reqs[i].Call = tr.Request, true, call
			return reqs
		}
	}
	return append(reqs, Request{ID: tr.Request, Prompt: tr.Prompt, Turn: tr.Turn, Call: call, Hook: tr.Waits, Since: now.UTC().Truncate(time.Second)})
}

// asked is the tool call an Ask can only be about: the one it names, or else
// the one running call of the prompt's tool by the agent that asked; "" when
// it may be about several, or none is known.
func asked(calls []adapter.Call, tr adapter.Transition) string {
	if tr.Call.ID != "" {
		return tr.Call.ID
	}
	id := ""
	for _, c := range calls {
		if c.Agent != tr.Prompt.Agent || c.Tool != tr.Prompt.Tool {
			continue
		}
		if id != "" {
			return ""
		}
		id = c.ID
	}
	return id
}

// running records that a main-agent call proves a turn is under way: in a
// session first seen in the middle of one, or in a turn that started without
// a prompt of its own, as Claude's queued prompts do.
func (r *Record) running(main bool) {
	if main && r.Main != adapter.Working {
		r.Main, r.MainKind, r.Reason = adapter.Working, "", ""
	}
}

// settle closes what agent was doing: its calls are over and whatever it
// asked the user is settled, as is any prompt that named no tool.
func (r *Record) settle(agent string) {
	r.Calls = slices.DeleteFunc(r.Calls, func(o adapter.Call) bool { return o.Agent == agent })
	r.Requests = slices.DeleteFunc(r.Requests, func(q Request) bool { return q.Prompt.Agent == agent || q.Prompt.Tool == "" })
}

func startCall(calls []adapter.Call, done []string, c adapter.Call) []adapter.Call {
	if c.ID == "" || slices.Contains(done, c.ID) || slices.ContainsFunc(calls, func(o adapter.Call) bool { return o.ID == c.ID }) {
		return calls
	}
	return append(calls, c)
}

// asks reports whether a request names prompt p.
func asks(reqs []Request, p adapter.Prompt) bool {
	return slices.ContainsFunc(reqs, func(q Request) bool { return q.Prompt == p })
}

// closePrompt closes prompt p: every request naming it.
func closePrompt(reqs []Request, p adapter.Prompt) []Request {
	return slices.DeleteFunc(reqs, func(q Request) bool { return q.Prompt == p })
}

// closeRequest closes one request. A later request on the same prompt takes
// its place: the prompt stays open, where it first opened.
func closeRequest(reqs []Request, id string) []Request {
	i := slices.IndexFunc(reqs, func(q Request) bool { return q.ID == id })
	if i < 0 {
		return reqs
	}
	p := reqs[i].Prompt
	if j := slices.IndexFunc(reqs[i+1:], func(q Request) bool { return q.Prompt == p }); j >= 0 {
		reqs[i] = reqs[i+1+j]
		return slices.Delete(reqs, i+1+j, i+2+j)
	}
	return slices.Delete(reqs, i, i+1)
}

// answerUnnamed closes the oldest prompt that named no tool. It says nothing
// about the tool, and a reminder does not reliably name the agent that
// asked: any finished tool answers it.
func answerUnnamed(reqs []Request) []Request {
	if i := slices.IndexFunc(reqs, func(q Request) bool { return q.Prompt.Tool == "" }); i >= 0 {
		return closePrompt(reqs, reqs[i].Prompt)
	}
	return reqs
}

func normalize(r *Record) {
	r.Shown = newest(validShown(r.Shown), maxShown)
	r.Requests = newest(r.Requests, maxRequests)
	r.Calls = newest(r.Calls, maxCalls)
	r.Done = newest(r.Done, maxDone)
	r.Background = newest(r.Background, maxTasks)
	r.Subagents = newest(r.Subagents, maxSubagents)
}

// validShown keeps the entries of s a hook could prove.
func validShown(s []Shown) []Shown {
	return slices.DeleteFunc(slices.Clone(s), func(e Shown) bool { return !e.Valid() })
}

// newest keeps the last n entries of s, and none as nil, so that records
// that hold the same store alike.
func newest[S ~[]E, E any](s S, n int) S {
	switch {
	case len(s) == 0:
		return nil
	case len(s) > n:
		return slices.Clone(s[len(s)-n:])
	}
	return s
}

// Same reports whether r and o hold the same session as they would be
// stored, contact time aside.
func (r Record) Same(o Record) bool {
	r.LastHookAt, o.LastHookAt = time.Time{}, time.Time{}
	a, errA := json.Marshal(r)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}
