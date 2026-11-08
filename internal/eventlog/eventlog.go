// Package eventlog is the append-only, sequence-numbered record of agent events.
//
// Writers and replay capture share an exclusive flock; callbacks run outside
// it. The log holds ids, states and timestamps only — never content.
package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

// PaneRef locates a pane on a specific tmux server.
type PaneRef struct {
	Server string `json:"server"`
	ID     string `json:"id"`
}

// Actor is the agent a control verb came from: one whose tool ran tmag.
// Session is the session it ran for, and Subagent the subagent, when the
// process table and the agent's own variables tell; Session is "" when the
// agent's process runs several sessions and nothing tells which.
type Actor struct {
	Agent    string `json:"agent"`
	Session  string `json:"session,omitempty"`
	Subagent string `json:"subagent,omitempty"`
}

// Transcript is where an agent writes a session's transcript, in the agent's
// own format.
type Transcript struct {
	Path   string `json:"path"`
	Format string `json:"format"` // claude-jsonl or codex-rollout
}

// Request is a prompt a session waits on the user for, under the id tmag
// answers it by.
type Request struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`            // permission, question or plan
	Tool  string    `json:"tool,omitempty"`  // the tool asked about
	Agent string    `json:"agent,omitempty"` // the subagent that asked
	Since time.Time `json:"since"`
}

// Event is one line of the log: a session's state changed (Event names the
// hook), tmag sent it a prompt, a pane started or stopped showing it, or it
// ended.
//
// An event that carries State reports the session in full: State, Kind, Turn,
// Cwd, Runtime, Transcript, Subagents, Capabilities and Requests are then what
// the session is, and a field left out is empty.
type Event struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Agent   string    `json:"agent"`
	Session string    `json:"session"`
	Event   string    `json:"event"`
	State   string    `json:"state,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	// Since is when the reported state/kind last changed. Older records may
	// omit it; Time remains when this event was appended, including repair.
	Since *time.Time `json:"since,omitempty"`
	Turn  string     `json:"turn,omitempty"`
	Cwd   string     `json:"cwd,omitempty"`
	// Runtime is how the session runs: terminal or service.
	Runtime    string      `json:"runtime,omitempty"`
	Transcript *Transcript `json:"transcript,omitempty"`
	// Subagents are the session's running subagents.
	Subagents []string `json:"subagents,omitempty"`
	// Capabilities are the verbs some channel carries to the session.
	Capabilities []string `json:"capabilities,omitempty"`
	// Requests are the prompts the session waits on the user for.
	Requests []Request `json:"requests,omitempty"`
	// Pane is the pane an Attach, Detach, Send or Halt is about.
	Pane *PaneRef `json:"pane,omitempty"`
	// Evidence is what an Attach rests on.
	Evidence string `json:"evidence,omitempty"`
	// From is the pane a Send, Halt or Answer came from, and By the agent,
	// when an agent's tool ran it. Bytes is a prompt's size.
	// Credential identifies an authenticated owner client, independently of By.
	Credential string   `json:"credential,omitempty"`
	From       *PaneRef `json:"from,omitempty"`
	By         *Actor   `json:"by,omitempty"`
	Bytes      int      `json:"bytes,omitempty"`
	// Via is the channel tmag delivered by: pane, service or hook.
	Via string `json:"via,omitempty"`
	// Request is the request an Answer decided, and Decision how: allow or
	// deny.
	Request  string `json:"request,omitempty"`
	Decision string `json:"decision,omitempty"`
	cursor   Cursor
}

// Cursor is the exact checkpoint assigned by a successful Append. It is not
// serialized and is unset for events read from disk or an unsuccessful append.
func (e Event) Cursor() Cursor { return e.cursor }

// Events tmag itself records, beside those named after agent hooks.
const (
	Send   = "Send"   // tmag submitted a prompt to the session
	Halt   = "Halt"   // tmag interrupted the session's turn
	Answer = "Answer" // tmag answered a request of the session's
	Attach = "Attach" // a pane started showing the session
	Detach = "Detach" // a pane stopped showing the session
	Exit   = "Exit"   // the session's process ended without SessionEnd
	End    = "SessionEnd"
)

// Ended reports whether e ends its session.
func (e Event) Ended() bool { return e.Event == End || e.Event == Exit }

const (
	currentName = "events.jsonl"
	rotatedName = "events.1.jsonl"
	seqName     = "seq"
	lockName    = "lock"
)

// DefaultMaxBytes is the size at which the current file is rotated.
const DefaultMaxBytes = 8 << 20

// Log is a directory holding the current and one rotated event file.
type Log struct {
	Dir      string
	MaxBytes int64
}

// Open returns the log rooted at dir.
func Open(dir string) *Log {
	return &Log{Dir: dir, MaxBytes: DefaultMaxBytes}
}

func (l *Log) path(name string) string { return filepath.Join(l.Dir, name) }

// WithLock runs fn while holding the log's exclusive lock. Waiting for the lock
// ends with ctx, so a stuck holder cannot stall a hook.
func (l *Log) WithLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return lockfile.With(ctx, l.path(lockName), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	})
}

// Append reserves a sequence before writing a complete record. An interrupted
// reservation leaves an unused number; recovery never reuses that number.
func (l *Log) Append(ctx context.Context, e *Event) error {
	e.cursor = Cursor{}
	return l.WithLock(ctx, func() error {
		h, err := l.history(ctx)
		if err != nil {
			return err
		}
		if h.Origin == 0 && h.Allocated == 0 {
			h.Origin = baseSeq(time.Now())
		}
		h.Allocated = max(h.Allocated, h.Origin) + 1
		e.Seq = h.Allocated
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if len(line) > maxRecordBytes {
			return fmt.Errorf("event exceeds %d bytes", maxRecordBytes)
		}
		// Reserve through the legacy sequence file first, so an older writer
		// cannot reuse a number if this process dies before checkpointing it.
		if err := l.writeSeq(e.Seq); err != nil {
			return err
		}
		if err := l.saveHistory(ctx, h); err != nil {
			return err
		}
		if h.Current.Size > 0 && h.Current.Size+int64(len(line)) > l.MaxBytes {
			// The old rotated segment is about to disappear. Persist the retention
			// fence first, so death at any rename boundary cannot certify lost history.
			h.Floor = max(h.Floor, h.Rotated.Last)
			h.Dirty = true
			if err := l.saveHistory(ctx, h); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Rename(l.path(currentName), l.path(rotatedName)); err != nil {
				return err
			}
			h.Rotated, h.Current = h.Current, segment{}
			h.Dirty = false
			if err := l.saveHistory(ctx, h); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.OpenFile(l.path(currentName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(line); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// If publication of the checkpoint fails, leave the caller's pending
		// marker intact. The next reader verifies the complete record from disk.
		st, err := os.Stat(l.path(currentName))
		if err != nil {
			return err
		}
		first := h.Current.First
		if first == 0 {
			first = e.Seq
		}
		h.Current = segmentOf(st)
		h.Current.First, h.Current.Last = first, e.Seq
		h.Committed = e.Seq
		if err := l.saveHistory(ctx, h); err != nil {
			return err
		}
		e.cursor = h.cursor()
		return nil
	})
}

// baseSeq starts numbering from microseconds, below JSON's exact integer limit.
// Persisted fences handle retention and repair; internal cursors also identify
// the incarnation, without assuming the wall clock always moves forward.
func baseSeq(now time.Time) int64 { return now.UnixMicro() }

// writeSeq is called only under the log lock. Its fixed temporary pathname is
// owned by that lock, including after a writer dies.
func (l *Log) writeSeq(seq int64) error {
	tmp := l.path(seqName + ".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(seqName))
}

// Seq is a bounded compatibility helper for diagnostics and tests. It returns
// the allocation high-water mark, including unused reservations. Production
// operations pass their context to Head instead.
func (l *Log) Seq() (int64, error) {
	c, err := l.Head(context.Background())
	return c.Seq, err
}
