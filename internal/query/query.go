// Package query implements the read and control operations of the tmag contract.
package query

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/bind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/registry"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// PaneInfo locates an agent in tmux.
type PaneInfo struct {
	Server  string `json:"server"`
	ID      string `json:"id"`
	Target  string `json:"target"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title"`
	Command string `json:"command"`
}

// Agent is one coding agent found in a tmux pane.
type Agent struct {
	// ID is the agent's session (thread) id; empty while unbound.
	ID      string     `json:"id,omitempty"`
	Agent   string     `json:"agent"`
	State   string     `json:"state"`
	Kind    string     `json:"kind,omitempty"`
	Turn    string     `json:"turn,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	Binding string     `json:"binding"`
	Pane    PaneInfo   `json:"pane"`
}

// KindSubmitted marks an agent that tmag just sent a prompt to, until the
// agent's own hook confirms the turn or PendingWindow passes.
const KindSubmitted = "submitted"

// PendingWindow is how long a submitted prompt counts as starting a turn
// without confirmation. Input that starts no turn (a slash command, say)
// releases the agent when it passes.
const PendingWindow = 15 * time.Second

func pending(rec registry.Record, now time.Time) bool {
	return !rec.Sent.IsZero() && now.Sub(rec.Sent) < PendingWindow
}

// Agents lists agents across all panes. Records left by exited or replaced
// agents are ignored; agents without a live record are unbound and unknown.
func Agents(w *bind.World) []Agent {
	now := time.Now()
	var out []Agent
	for _, p := range w.Panes {
		kind := w.AgentIn(p)
		if kind == "" {
			continue
		}
		a := Agent{
			Agent: kind, State: adapter.Unknown, Binding: bind.Unbound,
			Pane: PaneInfo{
				Server: p.Server, ID: p.ID, Target: p.Target(),
				Cwd: p.Path, Title: p.Title, Command: p.Command,
			},
		}
		if rec, ok := w.Record(p); ok {
			a.ID, a.State, a.Kind, a.Turn, a.Binding = rec.Session, rec.State, rec.Kind, rec.Turn, rec.Binding
			if !rec.Since.IsZero() {
				since := rec.Since
				a.Since = &since
			}
			if pending(rec, now) && rec.State == adapter.Idle {
				a.State, a.Kind = adapter.Working, KindSubmitted
				sent := rec.Sent
				a.Since = &sent
			}
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Pane, out[j].Pane
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		return a.Target < b.Target
	})
	return out
}

// Errors returned by target resolution and waiting.
var (
	ErrNotFound  = errors.New("no such agent")
	ErrAmbiguous = errors.New("ambiguous agent id")
	ErrEnded     = errors.New("agent session ended")
)

var paneSpec = regexp.MustCompile(`^(?:(.+):)?(%\d+)$`)

// minIDPrefix is the shortest id prefix accepted as a target.
const minIDPrefix = 4

// Resolve finds the agent addressed by target: an id or id prefix, a pane id
// (%N, on the caller's server), or server:%N where server is a socket name or path.
func Resolve(agents []Agent, target string) (Agent, error) {
	if m := paneSpec.FindStringSubmatch(target); m != nil {
		server := tmux.DefaultServer().Socket
		if m[1] != "" {
			server = tmux.ServerByName(m[1]).Socket
		}
		for _, a := range agents {
			if a.Pane.Server == server && a.Pane.ID == m[2] {
				return a, nil
			}
		}
		return Agent{}, fmt.Errorf("%w in pane %s", ErrNotFound, target)
	}
	if len(target) < minIDPrefix {
		return Agent{}, fmt.Errorf("%w: id prefix %q is shorter than %d characters", ErrNotFound, target, minIDPrefix)
	}
	var matches []Agent
	for _, a := range agents {
		if a.ID != "" && strings.HasPrefix(a.ID, target) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return Agent{}, fmt.Errorf("%w: %s", ErrNotFound, target)
	case 1:
		return matches[0], nil
	}
	return Agent{}, fmt.Errorf("%w: %s matches %d agents", ErrAmbiguous, target, len(matches))
}

// Refusal reasons for Send.
const (
	RefuseUnbound    = "unbound"    // no verified identity for the target
	RefuseBusy       = "busy"       // the target is not idle
	RefuseBackground = "background" // the agent does not hold its terminal
	RefuseChanged    = "changed"    // the target changed between check and delivery
)

// RefusedError is returned when Send's safety gate rejects delivery.
type RefusedError struct {
	Reason string
	Detail string
}

func (e *RefusedError) Error() string { return fmt.Sprintf("refused (%s): %s", e.Reason, e.Detail) }

// Send delivers text to an idle, bound agent as one submitted prompt and
// returns the agent and the sequence number of the Send event.
func Send(ctx context.Context, log *eventlog.Log, target, text string, from *eventlog.PaneRef) (Agent, int64, error) {
	text = strings.TrimRight(tmux.SanitizePrompt(text), "\n")
	if strings.TrimSpace(text) == "" {
		return Agent{}, 0, errors.New("nothing to send")
	}
	w, err := bind.Load(ctx)
	if err != nil {
		return Agent{}, 0, err
	}
	a, err := Resolve(Agents(w), target)
	if err != nil {
		return Agent{}, 0, err
	}
	if a.Binding == bind.Unbound {
		return a, 0, &RefusedError{RefuseUnbound, fmt.Sprintf("%s in %s has no verified session", a.Agent, a.Pane.ID)}
	}
	if a.State != adapter.Idle {
		return a, 0, &RefusedError{RefuseBusy, fmt.Sprintf("%s is %s", a.ID, describe(a))}
	}
	if p, ok := w.Find(a.Pane.Server, a.Pane.ID); ok && !w.Foreground(p) {
		return a, 0, notForeground(a, p)
	}

	var seq int64
	err = withPaneLock(ctx, log.Dir, a.Pane, func() error {
		srv := tmux.Server{Socket: a.Pane.Server}
		// Re-verify against fresh state: the agent may have started a turn,
		// exited, been replaced, or received another prompt since the listing.
		fresh, err := bind.Load(ctx)
		if err != nil {
			return err
		}
		p, ok := fresh.Find(a.Pane.Server, a.Pane.ID)
		if !ok {
			return &RefusedError{RefuseChanged, fmt.Sprintf("pane %s is gone", a.Pane.ID)}
		}
		rec, live := fresh.Record(p)
		if !live || rec.Session != a.ID || rec.Agent != a.Agent {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s no longer runs session %s", a.Pane.ID, a.ID)}
		}
		if rec.State != adapter.Idle || pending(rec, time.Now()) {
			return &RefusedError{RefuseBusy, fmt.Sprintf("%s became busy", a.ID)}
		}
		if !fresh.Foreground(p) {
			return notForeground(a, p)
		}
		if err := srv.Submit(ctx, p.ID, text); err != nil {
			return err
		}
		// Hold off other senders until the agent's hook confirms the turn or
		// the pending window passes.
		rec.Sent = time.Now().UTC().Truncate(time.Second)
		if err := registry.Write(ctx, srv, p.ID, rec); err != nil {
			return err
		}
		ev := eventlog.Event{
			Agent: a.Agent, Session: a.ID, Event: "Send", Cwd: p.Path,
			Pane: &eventlog.PaneRef{Server: p.Server, ID: p.ID}, Binding: rec.Binding,
			From: from, Bytes: len(text),
		}
		if err := log.Append(ctx, &ev); err != nil {
			return err
		}
		seq = ev.Seq
		a.State, a.Kind, a.Turn, a.Since = adapter.Working, KindSubmitted, "", &rec.Sent
		return nil
	})
	return a, seq, err
}

func notForeground(a Agent, p tmux.Pane) error {
	return &RefusedError{RefuseBackground, fmt.Sprintf("%s in %s is not in the foreground; %s is", a.Agent, p.ID, p.Command)}
}

func describe(a Agent) string {
	if a.Kind != "" {
		return a.State + " (" + a.Kind + ")"
	}
	return a.State
}

// withPaneLock serialises senders to one pane across processes.
func withPaneLock(ctx context.Context, dir string, p PaneInfo, fn func() error) error {
	sum := sha256.Sum256([]byte(p.Server + "\x00" + p.ID))
	return lockfile.With(ctx, filepath.Join(dir, "locks", hex.EncodeToString(sum[:8])), fn)
}

// Sender returns the pane the calling process runs in, if any, for provenance.
func Sender(w *bind.World) *eventlog.PaneRef {
	if p, ok := bind.Enclosing(w, os.Getpid()); ok {
		return &eventlog.PaneRef{Server: p.Server, ID: p.ID}
	}
	return nil
}

// ErrNoTurn reports that a submitted prompt started no turn within the grace
// period, e.g. because it was a slash command.
var ErrNoTurn = errors.New("no turn started")

// livenessInterval is how often Wait checks that the agent still runs.
const livenessInterval = 2 * time.Second

// WaitOptions tune Wait.
type WaitOptions struct {
	States []string
	After  int64
	// StartGrace, when positive, fails with ErrNoTurn if the session logs no
	// event within it.
	StartGrace time.Duration
}

// Wait blocks until an event of agent a with seq > o.After reaches one of
// o.States, and returns it. It fails with ErrEnded when the session ends or
// the agent leaves its pane.
func Wait(ctx context.Context, log *eventlog.Log, a Agent, o WaitOptions) (eventlog.Event, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	want := map[string]bool{}
	for _, s := range o.States {
		want[s] = true
	}
	seen := make(chan struct{})
	go watchLiveness(ctx, cancel, a, seen, o.StartGrace)

	var found eventlog.Event
	errFound := errors.New("found")
	sawEvent := false
	err := log.Follow(ctx, o.After, 200*time.Millisecond, func(e eventlog.Event) error {
		if e.Session != a.ID {
			return nil
		}
		if !sawEvent {
			sawEvent = true
			close(seen)
		}
		if e.Event == "SessionEnd" {
			return ErrEnded
		}
		if want[e.State] {
			found = e
			return errFound
		}
		return nil
	})
	switch {
	case errors.Is(err, errFound):
		return found, nil
	case context.Cause(ctx) != nil && !errors.Is(context.Cause(ctx), context.Canceled):
		return eventlog.Event{}, context.Cause(ctx)
	}
	return eventlog.Event{}, err
}

func watchLiveness(ctx context.Context, cancel context.CancelCauseFunc, a Agent, seen <-chan struct{}, grace time.Duration) {
	var graceC <-chan time.Time
	if grace > 0 {
		graceC = time.After(grace)
	}
	tick := time.NewTicker(livenessInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-seen:
			seen, graceC = nil, nil
		case <-graceC:
			cancel(ErrNoTurn)
			return
		case <-tick.C:
			w, err := bind.Load(ctx)
			if err != nil {
				continue
			}
			p, ok := w.Find(a.Pane.Server, a.Pane.ID)
			if !ok {
				cancel(fmt.Errorf("%w: pane %s is gone", ErrEnded, a.Pane.ID))
				return
			}
			if rec, live := w.Record(p); !live || rec.Session != a.ID {
				cancel(fmt.Errorf("%w: %s no longer runs session %s", ErrEnded, a.Pane.ID, a.ID))
				return
			}
		}
	}
}

// Snapshot is the full current state, consistent with events after Seq.
type Snapshot struct {
	Seq    int64   `json:"seq"`
	Agents []Agent `json:"agents"`
}

// TakeSnapshot reads the sequence number before listing, so replaying events
// after Seq over the listing never loses an update.
func TakeSnapshot(ctx context.Context, log *eventlog.Log) (Snapshot, error) {
	seq, err := log.Seq()
	if err != nil {
		return Snapshot{}, err
	}
	w, err := bind.Load(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	agents := Agents(w)
	if agents == nil {
		agents = []Agent{}
	}
	return Snapshot{Seq: seq, Agents: agents}, nil
}

// SessionEvents returns the most recent events of one session, oldest first.
func SessionEvents(ctx context.Context, log *eventlog.Log, id string, limit int) ([]eventlog.Event, error) {
	all, _, err := log.Since(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []eventlog.Event
	for _, e := range all {
		if e.Session == id {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// IsAgentKind reports whether s names a supported agent.
func IsAgentKind(s string) bool { return s == proc.Claude || s == proc.Codex }
