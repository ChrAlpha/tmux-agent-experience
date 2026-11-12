// Package machine reaches tmag on one machine through its public contracts
// only: a command that runs tmag there (tmag/v2), such as `ssh box tmag`, or
// an endpoint of `tmag serve` (tmag/http/v2). Each call is one tmag verb,
// whose own gate decides on the machine.
package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Backend is how the gateway reaches tmag on a machine.
type Backend interface {
	// Watch sends each watch document after since, or from a snapshot when
	// since is negative, until ctx ends or the stream breaks, which it
	// returns. A document each refuses ends the watch with that error.
	Watch(ctx context.Context, since int64, each func(Doc) error) error
	// Call runs one verb and returns its tmag/v2 document. A refusal or
	// failure is an *Error.
	Call(ctx context.Context, op Op) (json.RawMessage, error)
}

// Machine is a named machine and how to reach it.
type Machine struct {
	Name string
	Backend
}

var nameRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// CheckName reports whether name can name a machine: up to 32 letters,
// digits, '.', '_' and '-'.
func CheckName(name string) error {
	if !nameRule.MatchString(name) {
		return fmt.Errorf("machine name %q: use up to 32 letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// Op is one verb the page asks for.
type Op struct {
	Verb     string // read, request, send, interrupt or answer
	Target   string
	Request  string            // request, answer
	Last     bool              // read: the agent's last reply
	Lines    int               // read: lines of the pane
	Text     string            // send
	Decision string            // answer: allow or deny
	Message  string            // answer: a denial's message
	Answers  map[string]string // answer: question text to the chosen answer
}

// args is op as tmag's command line, and the text it reads on stdin.
func (op Op) args() ([]string, string, error) {
	switch op.Verb {
	case "read":
		if op.Last {
			return []string{"read", "--json", "--last", "--", op.Target}, "", nil
		}
		return []string{"read", "--json", "--lines", fmt.Sprint(op.Lines), "--", op.Target}, "", nil
	case "request":
		return []string{"request", "--json", "--", op.Target, op.Request}, "", nil
	case "send":
		return []string{"send", "--json", "--", op.Target, "-"}, op.Text, nil
	case "interrupt":
		return []string{"interrupt", "--json", "--", op.Target}, "", nil
	case "answer":
		args := []string{"answer", "--json"}
		if op.Message != "" {
			args = append(args, "--message="+op.Message)
		}
		if op.Answers != nil {
			b, _ := json.Marshal(op.Answers)
			args = append(args, "--answers="+string(b))
		}
		return append(args, "--", op.Target, op.Request, op.Decision), "", nil
	}
	return nil, "", fmt.Errorf("unknown verb %q", op.Verb)
}

// Error is a verb tmag refused or could not carry out, or a machine the
// gateway could not reach.
type Error struct {
	// Code is tmag's error code (refused:busy, not_found, timeout, …), or
	// unreachable when no tmag answered.
	Code    string `json:"code"`
	Message string `json:"message"`
	// Next is what tmag says to do next, when it knows.
	Next string `json:"next,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Unreachable is the code of a machine that gave no tmag document.
const Unreachable = "unreachable"

// failure reads tmag's error document, or reports the machine unreachable
// with what it said last.
func failure(doc []byte, said string) *Error {
	var d struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(doc, &d) == nil && d.Error != nil && d.Error.Code != "" {
		d.Error.Next = next(said)
		return d.Error
	}
	if said = lastLine(said); said == "" {
		said = "no tmag answered"
	}
	return &Error{Code: Unreachable, Message: said}
}

// next is the advice on a `next: ` line of tmag's error output.
func next(said string) string {
	for line := range strings.Lines(said) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "next: "); ok {
			return rest
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Doc is one document of `tmag watch`.
type Doc struct {
	Type     string    `json:"type"` // snapshot, event or panes
	Seq      int64     `json:"seq"`
	Sessions []Session `json:"sessions"`
	Panes    []Pane    `json:"panes"`
	Event    *Event    `json:"event"`
}

// Session is a session as tmag reports it, with the fields the page uses.
type Session struct {
	ID           string     `json:"id"`
	Agent        string     `json:"agent"`
	State        string     `json:"state"`
	Kind         string     `json:"kind,omitempty"`
	Turn         string     `json:"turn,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	Cwd          string     `json:"cwd,omitempty"`
	Runtime      string     `json:"runtime,omitempty"`
	Subagents    []string   `json:"subagents,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
	Requests     []Request  `json:"requests,omitempty"`
}

// Request is a prompt a session waits on the person for.
type Request struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool,omitempty"`
	Agent string    `json:"agent,omitempty"`
	Since time.Time `json:"since"`
}

// Pane is a tmux pane running an agent, and the session it shows.
type Pane struct {
	Server  string `json:"server"`
	ID      string `json:"id"`
	Target  string `json:"target"`
	Command string `json:"command,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	View    string `json:"view,omitempty"`
}

// Event is a line of tmag's event log.
type Event struct {
	Seq          int64      `json:"seq"`
	Time         time.Time  `json:"time"`
	Agent        string     `json:"agent"`
	Session      string     `json:"session"`
	Event        string     `json:"event"`
	State        string     `json:"state,omitempty"`
	Kind         string     `json:"kind,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	Turn         string     `json:"turn,omitempty"`
	Cwd          string     `json:"cwd,omitempty"`
	Runtime      string     `json:"runtime,omitempty"`
	Subagents    []string   `json:"subagents,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
	Requests     []Request  `json:"requests,omitempty"`
	Alert        *Alert     `json:"alert,omitempty"`
}

// Alert is what an Alert event tells the person about.
type Alert struct {
	Kind    string   `json:"kind"` // blocked or finished
	Turn    string   `json:"turn,omitempty"`
	Request *Request `json:"request,omitempty"`
	// Present is whether the person was at a terminal that shows the
	// session.
	Present bool `json:"present"`
}

// Ended reports whether e ends its session.
func (e *Event) Ended() bool { return e.Event == "SessionEnd" || e.Event == "Exit" }

// Reported is the session an event with a state reports in full.
func (e *Event) Reported() Session {
	return Session{ID: e.Session, Agent: e.Agent, State: e.State, Kind: e.Kind, Turn: e.Turn, Since: e.Since, Cwd: e.Cwd,
		Runtime: e.Runtime, Subagents: e.Subagents, Capabilities: e.Capabilities, Requests: e.Requests}
}
