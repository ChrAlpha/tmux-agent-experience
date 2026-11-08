// Package answer connects `tmag answer` to an answer hook: a Claude Code
// PermissionRequest hook that waits, while the agent's own dialog is on
// screen, for a decision made elsewhere.
//
// Each waiting hook listens on a Unix socket named after its request under
// the state directory. A connection sends one JSON object and reads one:
//
//	{"op":"show"}                 → {"ok":true,"tool":…,"input":…}
//	{"op":"decide","decision":…}  → {"ok":true} once the hook has printed it
//	{"op":"close"}                → {"ok":true}; the prompt closed elsewhere
//
// The tool input is the one piece of content tmag reads: the hook holds it in
// memory to show it and to hand it back to the agent, and writes it nowhere.
package answer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Option is the tmux option that says who may answer through tmag.
const Option = "@tmag-answer"

// Who may answer, as Option sets it.
const (
	Off    = "off"    // nobody: prompts are answered only in the agent
	People = "on"     // people, scripts and remote clients, but no agent
	Agents = "agents" // agents too, for one another
)

// Mode is who may answer, across the tmux servers tmag finds: the least any
// of them allows. Sessions outside tmux follow those servers; with none,
// and for a server that does not say, it is People.
func Mode(ctx context.Context) string {
	mode := ""
	servers, err := tmux.ServersContext(ctx)
	if err != nil {
		return Off
	}
	for _, srv := range servers {
		switch srv.GlobalOption(ctx, Option) {
		case Off:
			return Off
		case Agents:
			if mode == "" {
				mode = Agents
			}
		default:
			mode = People
		}
	}
	if mode == "" {
		return People
	}
	return mode
}

// Path is the socket of the hook waiting on request id.
func Path(stateDir, id string) string { return filepath.Join(stateDir, "req", id+".sock") }

// Asked is what a request asks: the tool and its input, as the agent gave
// them.
type Asked struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Decision is how the person decided.
type Decision struct {
	Behavior string            `json:"behavior"`          // allow or deny
	Message  string            `json:"message,omitempty"` // told to the agent on deny
	Answers  map[string]string `json:"answers,omitempty"` // question → chosen label, for a question
}

// Behaviors.
const (
	Allow = "allow"
	Deny  = "deny"
)

// Claude Code's tools whose approval must hand their input back.
const (
	askUserQuestion = "AskUserQuestion"
	exitPlanMode    = "ExitPlanMode"
)

// ErrInvalid marks a decision the request cannot take.
var ErrInvalid = errors.New("invalid decision")

// Output is the hook output that carries d to Claude Code. A question takes
// answers, keyed by question text, merged into its input; a plan's approval
// hands its input back unchanged, as Claude Code 2.1.317 requires.
func Output(a Asked, d Decision) ([]byte, error) {
	dec := map[string]any{"behavior": d.Behavior}
	switch d.Behavior {
	case Allow:
		if d.Message != "" {
			return nil, fmt.Errorf("%w: a message goes with deny only", ErrInvalid)
		}
		switch a.Tool {
		case askUserQuestion:
			input := map[string]any{}
			if err := json.Unmarshal(a.Input, &input); err != nil {
				return nil, fmt.Errorf("the question's input is not an object: %w", err)
			}
			if err := answersFit(a.Input, d.Answers); err != nil {
				return nil, err
			}
			input["answers"] = d.Answers
			dec["updatedInput"] = input
		case exitPlanMode:
			if len(a.Input) > 0 {
				dec["updatedInput"] = a.Input
			}
		}
		if a.Tool != askUserQuestion && len(d.Answers) > 0 {
			return nil, fmt.Errorf("%w: only a question takes answers", ErrInvalid)
		}
	case Deny:
		if len(d.Answers) > 0 {
			return nil, fmt.Errorf("%w: answers go with allow only", ErrInvalid)
		}
		if d.Message != "" {
			dec["message"] = d.Message
		}
	default:
		return nil, fmt.Errorf("%w: behavior %q is neither allow nor deny", ErrInvalid, d.Behavior)
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": dec}})
}

// answersFit checks that answers answer every question of the input, keyed
// by question text, each with a label: Claude Code drops answers to
// questions it did not ask and counts the question as unanswered.
func answersFit(input json.RawMessage, answers map[string]string) error {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(input, &in); err != nil || len(in.Questions) == 0 {
		return fmt.Errorf("%w: the request lists no questions", ErrInvalid)
	}
	asked := map[string]bool{}
	for _, q := range in.Questions {
		asked[q.Question] = true
	}
	for q := range answers {
		if !asked[q] {
			return fmt.Errorf("%w: %q is not one of the questions", ErrInvalid, q)
		}
	}
	for _, q := range in.Questions {
		if answers[q.Question] == "" {
			return fmt.Errorf("%w: no answer to %q; answer each question, keyed by its text", ErrInvalid, q.Question)
		}
	}
	return nil
}

type message struct {
	Op       string    `json:"op"`
	Request  string    `json:"request,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
}

type reply struct {
	OK      bool   `json:"ok"`
	Request string `json:"request,omitempty"`
	Error   string `json:"error,omitempty"`
	Asked
}

// Wait serves the request until a decision is printed through emit, the
// prompt closes, or ctx ends. It returns whether it emitted a decision; once
// ctx has ended it emits none, even for a connection it was serving.
func Wait(ctx context.Context, ln net.Listener, a Asked, emit func([]byte) error) bool {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return false
		}
		id := ""
		require := false
		if own, ok := ln.(*listener); ok {
			id = own.id
			require = own.slot != nil
		}
		done, emitted := serve(ctx, conn, id, require, a, emit)
		if done {
			return emitted
		}
	}
}

// serve answers one connection; done says the wait is over.
func serve(ctx context.Context, conn net.Conn, id string, require bool, a Asked, emit func([]byte) error) (done, emitted bool) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var m message
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&m); err != nil {
		return false, false
	}
	enc := json.NewEncoder(conn)
	if (require || m.Request != "") && m.Request != id {
		_ = enc.Encode(reply{Error: "request identity changed"})
		return false, false
	}
	switch m.Op {
	case "probe":
		_ = enc.Encode(struct {
			OK      bool   `json:"ok"`
			Request string `json:"request"`
		}{true, id})
		return false, false
	case "show":
		_ = enc.Encode(reply{OK: true, Request: id, Asked: a})
		return false, false
	case "close":
		_ = enc.Encode(reply{OK: true, Request: id})
		return true, false
	case "decide":
		if m.Decision == nil {
			_ = enc.Encode(reply{Error: "no decision"})
			return false, false
		}
		out, err := Output(a, *m.Decision)
		if err != nil {
			_ = enc.Encode(reply{Error: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")})
			return false, false
		}
		if ctx.Err() != nil {
			return true, false // the hook is leaving; the connection is closed
		}
		if err := emit(out); err != nil {
			_ = enc.Encode(reply{Error: "printing the decision failed: " + err.Error()})
			return true, false
		}
		_ = enc.Encode(reply{OK: true, Request: id})
		return true, true
	}
	_ = enc.Encode(reply{Error: "unknown op " + m.Op})
	return false, false
}

// ErrGone means no hook waits on the request any longer.
var ErrGone = errors.New("no answer hook waits on the request")

func call(ctx context.Context, stateDir, id string, m message) (reply, error) {
	scope, name, pooled, err := lookup(ctx, stateDir, id)
	if err != nil {
		return reply{}, fmt.Errorf("%w: %w", ErrGone, err)
	}
	defer scope.Close()
	m.Request = id
	conn, err := dialAt(ctx, scope, name)
	if err != nil {
		return reply{}, fmt.Errorf("%w: %w", ErrGone, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(m); err != nil {
		return reply{}, err
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return reply{}, fmt.Errorf("%w: it left without replying", ErrGone)
	}
	if r.OK && (pooled || r.Request != "") && r.Request != id {
		return r, fmt.Errorf("%w: request identity changed", ErrGone)
	}
	if !r.OK {
		return r, fmt.Errorf("%w: %s", ErrInvalid, r.Error)
	}
	return r, nil
}

// Show returns what the request asks.
func Show(ctx context.Context, stateDir, id string) (Asked, error) {
	r, err := call(ctx, stateDir, id, message{Op: "show"})
	return r.Asked, err
}

// Decide hands d to the hook and returns once the hook has printed it.
func Decide(ctx context.Context, stateDir, id string, d Decision) error {
	_, err := call(ctx, stateDir, id, message{Op: "decide", Decision: &d})
	return err
}

// Close tells a hook still waiting that its prompt closed, so it leaves. A
// request no hook waits on is left alone.
func Close(ctx context.Context, stateDir, id string) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if Probe(ctx, stateDir, id) != Dead {
		_, _ = call(ctx, stateDir, id, message{Op: "close"})
	}
}
