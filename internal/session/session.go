// Package session holds the state of agent sessions, wherever they run.
//
// A session is what an agent's hooks report on: one Claude Code session or
// Codex thread, identified by its agent and id. Its state never depends on a
// terminal; which panes show it is the business of package attach.
package session

import (
	"crypto/rand"
	"encoding/hex"
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

	// Main is the main agent's own state (working, idle or unknown) and
	// MainKind qualifies it (error after a failed turn).
	Main     string `json:"main"`
	MainKind string `json:"main_kind,omitempty"`
	// Turn is the main agent's current or last turn.
	Turn string `json:"turn,omitempty"`

	// Pending are the prompts awaiting the user, oldest first.
	Pending []adapter.Prompt `json:"pending,omitempty"`
	// Requests name the pending prompts, so they can be answered: each has
	// one, and a prompt two answer hooks wait on has two. A request closes
	// with its prompt.
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
	// Halted is when tmag last interrupted the main agent, until the agent
	// reports again: Claude Code reports nothing on an interrupt.
	Halted time.Time `json:"halted,omitzero"`
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
		State: r.State(), Kind: r.Kind(), Turn: r.Turn, Cwd: r.Cwd, Runtime: string(r.Host.Role),
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
	ev.State, ev.Kind, ev.Turn, ev.Cwd = r.State(), r.Kind(), r.Turn, r.Cwd
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
	maxPending   = 32
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
	case len(r.Pending) > 0:
		return adapter.Blocked, r.Pending[0].Kind()
	case r.Main == adapter.Working:
		return adapter.Working, ""
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
	rec.Pending = slices.Clone(cur.Pending)
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
		if tr.Fresh {
			rec.Background, rec.Turn = nil, ""
		}
		if tr.Background != nil {
			rec.Background = slices.Clone(*tr.Background)
		}
		keep := func(agent string) bool {
			return agent != "" && (tr.KeepSubagents || slices.Contains(rec.Background, agent))
		}
		rec.Pending = slices.DeleteFunc(rec.Pending, func(p adapter.Prompt) bool { return !keep(p.Agent) })
		rec.Calls = slices.DeleteFunc(rec.Calls, func(c adapter.Call) bool { return !keep(c.Agent) })
		rec.Done = nil
		rec.Main, rec.MainKind = tr.State, tr.Kind
		if tr.State == adapter.Working {
			rec.Sent = time.Time{} // the turn confirms the prompt tmag sent
		}
	case adapter.Ask:
		// A reminder cannot be told apart from a prompt already pending, and
		// outside a turn it cannot be about one.
		if tr.Reminder && (len(cur.Pending) > 0 || cur.State() != adapter.Working) {
			return cur, false
		}
		rec.Calls = startCall(rec.Calls, rec.Done, tr.Call)
		opened := !slices.Contains(rec.Pending, tr.Prompt)
		if opened {
			rec.Pending = append(rec.Pending, tr.Prompt)
		}
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
		if i := slices.Index(rec.Pending, adapter.Prompt{Agent: c.Agent, Tool: c.Tool}); i >= 0 && !running {
			rec.Pending = slices.Delete(rec.Pending, i, i+1)
		} else {
			rec.Pending = answerUnnamed(rec.Pending)
		}
	case adapter.Settle:
		rec.settle(tr.Call.Agent)
	case adapter.Answer:
		if tr.Request != "" {
			// Decided through tmag: the request closes, and its prompt with
			// it unless another request waits on the same prompt.
			q, ok := rec.Request(tr.Request)
			if !ok {
				return cur, false
			}
			rec.Requests = slices.DeleteFunc(rec.Requests, func(o Request) bool { return o.ID == q.ID })
			if !slices.ContainsFunc(rec.Requests, func(o Request) bool { return o.Prompt == q.Prompt }) {
				rec.Pending = slices.DeleteFunc(rec.Pending, func(p adapter.Prompt) bool { return p == q.Prompt })
			}
			break
		}
		if i := slices.Index(rec.Pending, tr.Prompt); i >= 0 {
			rec.Pending = slices.Delete(rec.Pending, i, i+1)
		} else {
			rec.Pending = answerUnnamed(rec.Pending)
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
	}
	normalize(&rec)
	if rec.State() != cur.State() || rec.Kind() != cur.Kind() || cur.Since.IsZero() {
		rec.Since = now.UTC().Truncate(time.Second)
	}
	return rec, !rec.equal(cur)
}

// request names the prompt an Ask is about. A prompt without a request
// gets the Ask's id; an answer hook's id names the prompt's request no hook
// waits on, or else adds one: two hooks may wait on equal prompts. Each Ask
// asks anew, so the request it names is about call from now on.
func request(reqs []Request, tr adapter.Transition, call string, now time.Time) []Request {
	named := slices.ContainsFunc(reqs, func(q Request) bool { return q.Prompt == tr.Prompt })
	if tr.Request == "" || (named && !tr.Waits) {
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
		r.Main, r.MainKind = adapter.Working, ""
	}
}

// settle closes what agent was doing: its calls are over and whatever it
// asked the user is settled, as is any prompt that named no tool.
func (r *Record) settle(agent string) {
	r.Calls = slices.DeleteFunc(r.Calls, func(o adapter.Call) bool { return o.Agent == agent })
	r.Pending = slices.DeleteFunc(r.Pending, func(p adapter.Prompt) bool { return p.Agent == agent || p.Tool == "" })
}

func startCall(calls []adapter.Call, done []string, c adapter.Call) []adapter.Call {
	if c.ID == "" || slices.Contains(done, c.ID) || slices.ContainsFunc(calls, func(o adapter.Call) bool { return o.ID == c.ID }) {
		return calls
	}
	return append(calls, c)
}

// answerUnnamed closes a prompt that named no tool. It says nothing about the
// tool, and a reminder does not reliably name the agent that asked: any
// finished tool answers it.
func answerUnnamed(pending []adapter.Prompt) []adapter.Prompt {
	if i := slices.IndexFunc(pending, func(p adapter.Prompt) bool { return p.Tool == "" }); i >= 0 {
		return slices.Delete(pending, i, i+1)
	}
	return pending
}

func normalize(r *Record) {
	r.Pending = newest(r.Pending, maxPending)
	// A request closes with its prompt.
	r.Requests = slices.DeleteFunc(r.Requests, func(q Request) bool { return !slices.Contains(r.Pending, q.Prompt) })
	r.Requests = newest(r.Requests, maxRequests)
	r.Calls = newest(r.Calls, maxCalls)
	r.Done = newest(r.Done, maxDone)
	r.Background = newest(r.Background, maxTasks)
	r.Subagents = newest(r.Subagents, maxSubagents)
	if len(r.Pending) == 0 {
		r.Pending = nil
	}
	if len(r.Requests) == 0 {
		r.Requests = nil
	}
	if len(r.Calls) == 0 {
		r.Calls = nil
	}
	if len(r.Done) == 0 {
		r.Done = nil
	}
	if len(r.Background) == 0 {
		r.Background = nil
	}
	if len(r.Subagents) == 0 {
		r.Subagents = nil
	}
}

// newest keeps the last n entries of s.
func newest[S ~[]E, E any](s S, n int) S {
	if len(s) > n {
		return slices.Clone(s[len(s)-n:])
	}
	return s
}

func (r Record) equal(o Record) bool {
	return r.Agent == o.Agent && r.ID == o.ID && r.Cwd == o.Cwd && r.Host == o.Host && r.Transcript == o.Transcript &&
		r.Main == o.Main && r.MainKind == o.MainKind && r.Turn == o.Turn &&
		slices.Equal(r.Pending, o.Pending) && slices.Equal(r.Requests, o.Requests) && slices.Equal(r.Calls, o.Calls) &&
		slices.Equal(r.Done, o.Done) && slices.Equal(r.Background, o.Background) &&
		slices.Equal(r.Subagents, o.Subagents) && r.Since.Equal(o.Since) && r.Sent.Equal(o.Sent) && r.Halted.Equal(o.Halted)
}
