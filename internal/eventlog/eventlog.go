// Package eventlog is the append-only, sequence-numbered record of agent events.
//
// Hooks append under an exclusive flock; readers never lock. The log holds ids,
// states and timestamps only — never prompt text or agent output.
package eventlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

// PaneRef locates a pane on a specific tmux server.
type PaneRef struct {
	Server string `json:"server"`
	ID     string `json:"id"`
}

// Transcript is where an agent writes a session's transcript, in the agent's
// own format.
type Transcript struct {
	Path   string `json:"path"`
	Format string `json:"format"` // claude-jsonl or codex-rollout
}

// Event is one line of the log: a session's state changed (Event names the
// hook), tmag sent it a prompt, a pane started or stopped showing it, or it
// ended.
//
// An event that carries State reports the session in full: State, Kind, Turn,
// Cwd, Runtime, Transcript and Subagents are then what the session is, and a
// field left out is empty.
type Event struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Agent   string    `json:"agent"`
	Session string    `json:"session"`
	Event   string    `json:"event"`
	State   string    `json:"state,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Turn    string    `json:"turn,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	// Runtime is how the session runs: terminal or service.
	Runtime    string      `json:"runtime,omitempty"`
	Transcript *Transcript `json:"transcript,omitempty"`
	// Subagents are the session's running subagents.
	Subagents []string `json:"subagents,omitempty"`
	// Pane is the pane an Attach, Detach or Send is about.
	Pane *PaneRef `json:"pane,omitempty"`
	// Evidence is what an Attach rests on.
	Evidence string `json:"evidence,omitempty"`
	// From is the pane a Send came from, and Bytes the prompt's size.
	From  *PaneRef `json:"from,omitempty"`
	Bytes int      `json:"bytes,omitempty"`
}

// Events tmag itself records, beside those named after agent hooks.
const (
	Send   = "Send"   // tmag submitted a prompt to the session
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

// DefaultDir resolves the state directory: $TMAG_STATE_DIR, then
// $XDG_STATE_HOME/tmag, then ~/.local/state/tmag.
func DefaultDir() string {
	if d := os.Getenv("TMAG_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tmag")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "state", "tmag")
}

func (l *Log) path(name string) string { return filepath.Join(l.Dir, name) }

// WithLock runs fn while holding the log's exclusive lock. Waiting for the lock
// ends with ctx, so a stuck holder cannot stall a hook.
func (l *Log) WithLock(ctx context.Context, fn func() error) error {
	return lockfile.With(ctx, l.path(lockName), fn)
}

// Append assigns the next sequence number and time to e and writes it.
//
// The new sequence number is committed before the line is written: a crash in
// between leaves a hole, which readers tolerate, rather than a reused number,
// which they would drop as a duplicate.
func (l *Log) Append(ctx context.Context, e *Event) error {
	return l.WithLock(ctx, func() error {
		seq, err := l.readSeq()
		if err != nil {
			return err
		}
		if seq == 0 {
			seq = baseSeq(time.Now())
		}
		e.Seq = seq + 1
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if err := l.writeSeq(e.Seq); err != nil {
			return err
		}
		if err := l.rotateIfNeeded(int64(len(line))); err != nil {
			return err
		}
		f, err := os.OpenFile(l.path(currentName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(line); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

// baseSeq numbers a new log from the current time, so a log recreated after a
// purge always numbers above any cursor a client kept from the old one, and
// such a cursor is recognised as stale instead of silently skipping events.
// Microsecond resolution stays below 2^53, exact in JSON clients.
func baseSeq(now time.Time) int64 {
	return now.UnixMicro()
}

func (l *Log) writeSeq(seq int64) error {
	tmp := l.path(seqName + ".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(seqName))
}

func (l *Log) rotateIfNeeded(incoming int64) error {
	st, err := os.Stat(l.path(currentName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Size()+incoming <= l.MaxBytes {
		return nil
	}
	return os.Rename(l.path(currentName), l.path(rotatedName))
}

// readSeq returns the last assigned sequence number. It falls back to scanning the
// files when the seq file is missing, so deleting it never reuses numbers.
func (l *Log) readSeq() (int64, error) {
	b, err := os.ReadFile(l.path(seqName))
	if err == nil {
		if n, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			return n, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var max int64
	for _, name := range []string{rotatedName, currentName} {
		evs, _, err := readFile(l.path(name), 0)
		if err != nil {
			return 0, err
		}
		for _, e := range evs {
			if e.Seq > max {
				max = e.Seq
			}
		}
	}
	return max, nil
}

// Seq returns the last assigned sequence number, 0 for an empty log.
func (l *Log) Seq() (int64, error) {
	return l.readSeq()
}

// Since returns events with seq > after in order. complete is false when the
// caller may have missed events: they were rotated away, or the log was
// recreated and `after` belongs to an older one.
func (l *Log) Since(ctx context.Context, after int64) (events []Event, complete bool, err error) {
	err = l.WithLock(ctx, func() error {
		oldest := int64(0)
		for _, name := range []string{rotatedName, currentName} {
			evs, min, err := readFile(l.path(name), after)
			if err != nil {
				return err
			}
			if oldest == 0 || (min != 0 && min < oldest) {
				oldest = min
			}
			events = append(events, evs...)
		}
		seq, err := l.readSeq()
		if err != nil {
			return err
		}
		switch {
		case after == seq:
			complete = true
		case after > seq || oldest == 0:
			complete = false
		default:
			complete = after+1 >= oldest
		}
		return nil
	})
	return events, complete, err
}

// readFile returns the events with seq > after and the smallest seq in the file.
func readFile(path string, after int64) ([]Event, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []Event
	var min int64
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var e Event
			if json.Unmarshal(line, &e) == nil {
				if min == 0 || e.Seq < min {
					min = e.Seq
				}
				if e.Seq > after {
					out = append(out, e)
				}
			}
		}
		if err == io.EOF {
			return out, min, nil
		}
		if err != nil {
			return nil, 0, err
		}
	}
}

// Follow calls fn for every event with seq > after, in order, as it is appended,
// until ctx is done or fn returns an error. Rotation and gaps are handled.
func (l *Log) Follow(ctx context.Context, after int64, interval time.Duration, fn func(Event) error) error {
	last := after
	emit := func(e Event) error {
		if e.Seq <= last {
			return nil
		}
		if e.Seq > last+1 {
			// A writer rotated between our reads; recover the missing range.
			missing, _, err := l.Since(ctx, last)
			if err != nil {
				return err
			}
			for _, m := range missing {
				if m.Seq >= e.Seq {
					break
				}
				if err := fn(m); err != nil {
					return err
				}
				last = m.Seq
			}
		}
		if err := fn(e); err != nil {
			return err
		}
		last = e.Seq
		return nil
	}

	backlog, _, err := l.Since(ctx, last)
	if err != nil {
		return err
	}
	for _, e := range backlog {
		if err := emit(e); err != nil {
			return err
		}
	}

	t := &tail{path: l.path(currentName)}
	defer t.close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		lines, err := t.poll()
		if err != nil {
			return err
		}
		for _, line := range lines {
			var e Event
			if json.Unmarshal(line, &e) != nil {
				continue
			}
			if err := emit(e); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tail reads complete lines appended to a file, following renames by inode.
type tail struct {
	path    string
	f       *os.File
	ino     uint64
	partial []byte
}

func (t *tail) close() {
	if t.f != nil {
		t.f.Close()
	}
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

func (t *tail) poll() ([][]byte, error) {
	var lines [][]byte
	if t.f != nil {
		more, err := t.drain()
		if err != nil {
			return nil, err
		}
		lines = append(lines, more...)
	}
	fi, err := os.Stat(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return lines, nil
	}
	if err != nil {
		return nil, err
	}
	if t.f == nil || inode(fi) != t.ino {
		t.close()
		f, err := os.Open(t.path)
		if errors.Is(err, os.ErrNotExist) {
			t.f = nil
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
		t.f, t.partial = f, nil
		if st, err := f.Stat(); err == nil {
			t.ino = inode(st)
		}
		more, err := t.drain()
		if err != nil {
			return nil, err
		}
		lines = append(lines, more...)
	}
	return lines, nil
}

func (t *tail) drain() ([][]byte, error) {
	data, err := io.ReadAll(t.f)
	if err != nil {
		return nil, err
	}
	data = append(t.partial, data...)
	var lines [][]byte
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, data[:i])
		data = data[i+1:]
	}
	t.partial = append([]byte(nil), data...)
	return lines, nil
}
