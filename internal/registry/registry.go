// Package registry stores the live agent record of a pane in tmux pane options.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Record is the live state of the agent bound to one pane.
type Record struct {
	Agent   string // claude | codex
	Session string // agent session / thread id
	PID     int    // the agent process the record belongs to
	State   string
	Kind    string
	Pending []adapter.Prompt // prompts awaiting the user while blocked, oldest first
	Base    string           // while blocked, the kind of work the agent returns to
	Calls   []adapter.Call   // tool calls started and not yet over
	Turn    string
	Since   time.Time // when State last changed
	Sent    time.Time // when tmag last submitted a prompt not yet confirmed by a hook
	Binding string    // evidence that bound Session to this pane
	Command string    // pane_current_command when bound; guards status-line display
}

// Bound reports whether the pane carries a record.
func (r Record) Bound() bool { return r.Session != "" && r.Agent != "" && r.PID > 0 }

// Equal reports whether two records hold the same values.
func (r Record) Equal(o Record) bool {
	return r.Agent == o.Agent && r.Session == o.Session && r.PID == o.PID &&
		r.State == o.State && r.Kind == o.Kind && slices.Equal(r.Pending, o.Pending) &&
		r.Base == o.Base && slices.Equal(r.Calls, o.Calls) &&
		r.Turn == o.Turn && r.Since.Equal(o.Since) && r.Sent.Equal(o.Sent) &&
		r.Binding == o.Binding && r.Command == o.Command
}

// FromPane decodes the record from a pane's tmag options.
func FromPane(p tmux.Pane) Record {
	o := p.Options
	r := Record{
		Agent: o["agent"], Session: o["session"], State: o["state"], Kind: o["kind"],
		Pending: decodePrompts(o["pending"]), Base: o["base"], Calls: decodeCalls(o["calls"]),
		Turn: o["turn"], Binding: o["bind"], Command: o["cmd"],
	}
	r.PID, _ = strconv.Atoi(o["pid"])
	r.Since = unix(o["since"])
	r.Sent = unix(o["sent"])
	return r
}

// Prompts are stored as space-separated agent/tool pairs; neither agent ids
// nor tool names contain either separator.
func encodePrompts(ps []adapter.Prompt) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.Agent + "/" + p.Tool
	}
	return strings.Join(parts, " ")
}

func decodePrompts(s string) []adapter.Prompt {
	var out []adapter.Prompt
	for _, f := range strings.Fields(s) {
		if agent, tool, ok := strings.Cut(f, "/"); ok {
			out = append(out, adapter.Prompt{Agent: agent, Tool: tool})
		}
	}
	return out
}

// Calls are stored as space-separated agent/tool/id triples.
func encodeCalls(cs []adapter.Call) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Agent + "/" + c.Tool + "/" + c.ID
	}
	return strings.Join(parts, " ")
}

func decodeCalls(s string) []adapter.Call {
	var out []adapter.Call
	for _, f := range strings.Fields(s) {
		if p := strings.SplitN(f, "/", 3); len(p) == 3 {
			out = append(out, adapter.Call{Agent: p[0], Tool: p[1], ID: p[2]})
		}
	}
	return out
}

func unix(s string) time.Time {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

// Write stores the record on the pane, clearing options that are empty.
func Write(ctx context.Context, s tmux.Server, pane string, r Record) error {
	values := map[string]string{
		"agent": r.Agent, "session": r.Session, "state": r.State, "kind": r.Kind,
		"pending": encodePrompts(r.Pending), "base": r.Base, "calls": encodeCalls(r.Calls),
		"turn": r.Turn, "bind": r.Binding, "cmd": r.Command,
	}
	if r.PID > 0 {
		values["pid"] = strconv.Itoa(r.PID)
	}
	if !r.Since.IsZero() {
		values["since"] = strconv.FormatInt(r.Since.Unix(), 10)
	}
	if !r.Sent.IsZero() {
		values["sent"] = strconv.FormatInt(r.Sent.Unix(), 10)
	}
	set := map[string]string{}
	var unset []string
	for _, k := range tmux.OptionKeys {
		if v := values[k]; v != "" {
			set[k] = v
		} else {
			unset = append(unset, k)
		}
	}
	return s.SetPaneOptions(ctx, pane, set, unset)
}

// MarkSent records that tmag submitted a prompt, leaving the rest of the
// record as it is.
func MarkSent(ctx context.Context, s tmux.Server, pane string, t time.Time) error {
	return s.SetPaneOptions(ctx, pane, map[string]string{"sent": strconv.FormatInt(t.Unix(), 10)}, nil)
}

// Lock runs fn holding the pane's record lock, a file under stateDir shared by
// every tmag process. Hooks and senders read, change and report a pane's
// record only under it, so neither overwrites an update it has not seen and
// the event log orders their events as they happened.
func Lock(ctx context.Context, stateDir, server, pane string, fn func() error) error {
	sum := sha256.Sum256([]byte(server + "\x00" + pane))
	return lockfile.With(ctx, filepath.Join(stateDir, "locks", hex.EncodeToString(sum[:8])), fn)
}
