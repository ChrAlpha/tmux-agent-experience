// Package registry stores the live agent record of a pane in tmux pane options.
package registry

import (
	"context"
	"strconv"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Record is the live state of the agent bound to one pane.
type Record struct {
	Agent   string // claude | codex
	Session string // agent session / thread id
	PID     int    // the agent process the record belongs to
	State   string
	Kind    string
	Tool    string // tool awaiting permission while blocked
	Turn    string
	Since   time.Time // when State last changed
	Sent    time.Time // when tmag last submitted a prompt not yet confirmed by a hook
	Binding string    // evidence that bound Session to this pane
	Command string    // pane_current_command when bound; guards status-line display
}

// Bound reports whether the pane carries a record.
func (r Record) Bound() bool { return r.Session != "" && r.Agent != "" && r.PID > 0 }

// FromPane decodes the record from a pane's tmag options.
func FromPane(p tmux.Pane) Record {
	o := p.Options
	r := Record{
		Agent: o["agent"], Session: o["session"], State: o["state"], Kind: o["kind"],
		Tool: o["tool"], Turn: o["turn"], Binding: o["bind"], Command: o["cmd"],
	}
	r.PID, _ = strconv.Atoi(o["pid"])
	r.Since = unix(o["since"])
	r.Sent = unix(o["sent"])
	return r
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
		"tool": r.Tool, "turn": r.Turn, "bind": r.Binding, "cmd": r.Command,
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
