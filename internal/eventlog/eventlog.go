// Package eventlog is the append-only, sequence-numbered record of agent
// events, which tells watchers what changed.
//
// The log is told of a change after it is made, best effort: a writer makes
// the change where it lives (a session's record, a pane's options) and then
// logs it, so a writer that dies in between leaves that change unlogged until
// the session's next event reports it. Sequence numbers run without gaps
// through the retained files, so a reader can always tell when it missed
// events, because they rotated away or the log was recreated, and then takes
// a fresh snapshot of the state instead (Tail). Writers append under one
// lock; readers take none. The log holds ids, states, paths and timestamps
// only, never content.
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
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
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
	Format string `json:"format"` // claude-jsonl, codex-rollout or pi-jsonl
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

// AlertInfo is what an Alert event reports: the change worth telling the
// person about, and whether they were at a terminal that showed the session
// when it happened.
type AlertInfo struct {
	Kind    string        `json:"kind"`              // blocked or finished
	Turn    string        `json:"turn,omitempty"`    // the turn that ended
	Request *AlertRequest `json:"request,omitempty"` // the request that just opened
	Present bool          `json:"present"`
}

// AlertRequest is the request a blocked alert reports, as the notify command
// is given it: the prompt that just opened.
type AlertRequest struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Tool string `json:"tool,omitempty"`
}

// Event is one line of the log: a session's state changed (Event names the
// hook), tmag sent it a prompt, a pane started or stopped showing it, a
// change was worth telling the person about, or it ended.
//
// An event that carries State reports the session in full: State, Kind, Turn,
// Cwd, Runtime, Transcript, Subagents, Capabilities and Requests are then what
// the session is, and a field left out is empty.
type Event struct {
	// LastHookAt is the session's last hook contact when the event was
	// logged.
	LastHookAt time.Time `json:"last_hook_at,omitzero"`
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"time"`
	Agent      string    `json:"agent"`
	Session    string    `json:"session"`
	Event      string    `json:"event"`
	State      string    `json:"state,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	// Since is when the reported state/kind last changed.
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
	// Alert is the change an Alert event tells the person about.
	Alert *AlertInfo `json:"alert,omitempty"`
	// Pane is the pane an Attach, Detach, Send or Halt is about.
	Pane *PaneRef `json:"pane,omitempty"`
	// Evidence is what an Attach rests on.
	Evidence string `json:"evidence,omitempty"`
	// From is the pane a Send, Halt or Answer came from, and By the agent,
	// when an agent's tool ran it. Bytes is a prompt's size. Credential
	// identifies an authenticated owner client, independently of By.
	Credential string   `json:"credential,omitempty"`
	From       *PaneRef `json:"from,omitempty"`
	By         *Actor   `json:"by,omitempty"`
	Bytes      int      `json:"bytes,omitempty"`
	// Via is the channel tmag delivered by: pane, service, hook or inbox.
	Via string `json:"via,omitempty"`
	// Request is the request an Answer decided, and Decision how: allow or
	// deny.
	Request  string `json:"request,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// Events tmag itself records, beside those named after agent hooks.
const (
	Send   = "Send"   // tmag sent the session a prompt
	Halt   = "Halt"   // tmag interrupted the session's turn
	Answer = "Answer" // tmag answered a request of the session's
	Attach = "Attach" // a pane started showing the session
	Detach = "Detach" // a pane stopped showing the session
	Exit   = "Exit"   // the session's process ended without SessionEnd
	End    = "SessionEnd"
	Alert  = "Alert" // a change worth telling the person about
)

// Ended reports whether e ends its session.
func (e Event) Ended() bool { return e.Event == End || e.Event == Exit }

const (
	currentName = "events.jsonl"
	rotatedName = "events.1.jsonl"
	lockName    = "lock"
)

// DefaultMaxBytes is the size at which the current file is rotated.
const DefaultMaxBytes = 8 << 20

// ReadTimeout bounds one read of the log, or of the state it reports on,
// rather than a stream's whole lifetime.
const ReadTimeout = 5 * time.Second

// maxLine bounds a line readers decode. What an event can hold is bounded far
// below it where hook input is read (adapter.Decode), so a longer line is
// damage, skipped like any line that does not decode.
const maxLine = 1 << 20

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

// WithLock runs fn while holding the log's lock, which every writer takes.
// Waiting for it ends with ctx, so a stuck holder cannot stall a hook.
func (l *Log) WithLock(ctx context.Context, fn func() error) error {
	return lockfile.With(ctx, l.path(lockName), fn)
}

// Append logs e with the next sequence number, and the time now unless e
// has one. A fresh log numbers from the current time in microseconds, so its
// numbers follow those of a log it replaces. A line a writer that died left
// unfinished is ended first, so that readers skip it whole; its number is
// used again, keeping the numbers readers see without gaps.
func (l *Log) Append(ctx context.Context, e *Event) error {
	return l.WithLock(ctx, func() error {
		f, err := l.openAppend()
		if err != nil {
			return err
		}
		defer func() { f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		last, end, err := lastLine(ctx, f, st.Size())
		if err != nil {
			return err
		}
		if last == 0 {
			if last, err = l.lastRotated(ctx); err != nil {
				return err
			}
		}
		if last == 0 {
			last = time.Now().UnixMicro()
		}
		e.Seq = last + 1
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		line = append(line, '\n')
		if st.Size() > 0 && st.Size()+int64(len(line)) > l.MaxBytes {
			f.Close()
			if err := os.Rename(l.path(currentName), l.path(rotatedName)); err != nil {
				return err
			}
			if f, err = l.openAppend(); err != nil {
				return err
			}
		} else if end != st.Size() {
			line = append([]byte{'\n'}, line...)
		}
		return contextio.WriteAll(ctx, f, line)
	})
}

func (l *Log) openAppend() (*os.File, error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(l.path(currentName), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
}

// lastRotated is the last sequence number of the rotated file, or 0.
func (l *Log) lastRotated(ctx context.Context) (int64, error) {
	f, err := os.Open(l.path(rotatedName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	last, _, err := lastLine(ctx, f, st.Size())
	return last, err
}

// scanBack reads the first size bytes of f from their end, in windows that
// double until fn, given a window and where it starts, is satisfied, or the
// window reaches the start of the file: a line may be as long as readers
// accept, so no fixed window is enough. The first window holds the last
// line of a healthy log.
func scanBack(ctx context.Context, f *os.File, size int64, fn func(buf []byte, base int64) bool) error {
	for window := int64(4 << 10); ; window *= 2 {
		n := min(size, window)
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fn(buf, size-n) || n == size {
			return nil
		}
	}
}

// lastLine returns the sequence number of the last line of f that decodes,
// 0 when none does, and the offset after its last complete line, 0 when it
// has none: a reader that has read every complete line stands there, and f
// ends a line when that offset is its size. Both come from one read of f's
// end, which holds them in a healthy log.
func lastLine(ctx context.Context, f *os.File, size int64) (seq, end int64, err error) {
	if size == 0 {
		return 0, 0, nil
	}
	err = scanBack(ctx, f, size, func(buf []byte, base int64) bool {
		seq, end = lastSeqIn(buf), 0
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			end = base + int64(i) + 1
		}
		return seq > 0 && end > 0
	})
	return seq, end, err
}

// lastSeqIn returns the sequence number of the last line in buf that
// decodes, or 0.
func lastSeqIn(buf []byte) int64 {
	end := len(buf)
	for end > 0 {
		start := end - 1
		for start > 0 && buf[start-1] != '\n' {
			start--
		}
		if seq := seqOf(buf[start:end]); seq > 0 {
			return seq
		}
		end = start
		for end > 0 && buf[end-1] == '\n' {
			end--
		}
	}
	return 0
}

func seqOf(line []byte) int64 {
	var e struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(line, &e) != nil {
		return 0
	}
	return e.Seq
}

// Head is the sequence number of the last event, or 0 for an empty log.
func (l *Log) Head(ctx context.Context) (int64, error) {
	f, err := os.Open(l.path(currentName))
	if errors.Is(err, os.ErrNotExist) {
		return l.lastRotated(ctx)
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	last, _, err := lastLine(ctx, f, st.Size())
	if err == nil && last == 0 {
		return l.lastRotated(ctx)
	}
	return last, err
}

// Since returns the retained events after seq after, oldest first, and
// whether they are all of them: false when some rotated away, the log was
// recreated or damaged since, or after is ahead of it. After 0 asks for
// everything retained, which is complete by definition.
func (l *Log) Since(ctx context.Context, after int64) ([]Event, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	var all []Event
	for _, name := range []string{rotatedName, currentName} {
		events, err := l.read(ctx, name)
		if err != nil {
			return nil, false, err
		}
		all = append(all, events...)
	}
	out, last := after1(after, all)
	if after == 0 {
		return out, true, nil
	}
	return out, continues(after, out) && (len(out) > 0 || last == after), nil
}

// after1 keeps the events after seq after, and returns the last sequence
// number among all of them.
func after1(after int64, events []Event) ([]Event, int64) {
	var out []Event
	last := int64(0)
	for _, e := range events {
		last = max(last, e.Seq)
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, last
}

// continues reports whether events follow seq after without a gap; after
// 0 asks only that they follow one another.
func continues(after int64, events []Event) bool {
	prev := after
	for i, e := range events {
		if (i > 0 || after != 0) && e.Seq != prev+1 {
			return false
		}
		prev = e.Seq
	}
	return true
}

// read decodes the complete lines of the named file, skipping those that
// do not decode.
func (l *Log) read(ctx context.Context, name string) ([]Event, error) {
	f, _, err := l.openRead(name)
	if err != nil || f == nil {
		return nil, err
	}
	defer f.Close()
	events, _, err := readFrom(ctx, f, 0)
	return events, err
}

// openRead opens the named file for reading and identifies it, or returns a
// nil file when there is none.
func (l *Log) openRead(name string) (*os.File, fileID, error) {
	f, err := os.Open(l.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fileID{}, nil
	}
	if err != nil {
		return nil, fileID{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fileID{}, err
	}
	return f, identity(st), nil
}

func identity(st os.FileInfo) fileID {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return fileID{dev: uint64(s.Dev), ino: s.Ino} //nolint:unconvert // Darwin Dev is int32; Linux uses uint64.
	}
	return fileID{}
}

// readFrom decodes the complete lines of f from offset at, skipping those
// that do not decode, and returns the offset after the last of them.
func readFrom(ctx context.Context, f *os.File, at int64) ([]Event, int64, error) {
	if _, err := f.Seek(at, io.SeekStart); err != nil {
		return nil, at, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var events []Event
	for {
		if err := ctx.Err(); err != nil {
			return nil, at, err
		}
		line, n, err := readLine(r)
		if errors.Is(err, io.EOF) {
			return events, at, nil // the end, or an unfinished last line
		}
		if err != nil {
			return nil, at, err
		}
		at += n
		if line == nil {
			continue // longer than maxLine
		}
		var e Event
		if json.Unmarshal(line, &e) == nil && e.Seq > 0 {
			events = append(events, e)
		}
	}
}

// readLine returns the next complete line and how many bytes it took, or a
// nil line for one longer than maxLine, which it consumes whole.
func readLine(r *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var n int64
	long := false
	for {
		part, err := r.ReadSlice('\n')
		n += int64(len(part))
		if !long {
			if len(line)+len(part) > maxLine {
				long, line = true, nil
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if long {
			return nil, n, nil
		}
		return line, n, nil
	}
}

// Tail follows the log, reading only what each file gained since it last
// looked. It holds the file it reads open, so that the file's inode stays
// its own whatever rotation does to its name, and it can read the rest of
// it after the file is rotated away. tmag never empties a file in place; a
// file emptied under a tail all the same, which keeps its inode, is shorter
// than where the tail stands, and costs a snapshot. Close releases the file.
type Tail struct {
	log  *Log
	seq  int64
	file *os.File // the file it reads; nil before the first look
	at   int64    // the offset it stands at in that file
}

// Tail returns a tail that delivers the events after seq after, which it
// vouches for by their numbers alone: 0 asks for everything retained.
func (l *Log) Tail(after int64) *Tail { return &Tail{log: l, seq: after} }

// Follow returns a tail at the log's end, which delivers the events logged
// after the call; Seq is the last before them, 0 for an empty log. It holds
// the current file, which it creates for an empty log, so that it vouches
// for every event written to it since, the first events of a log too.
func (l *Log) Follow(ctx context.Context) (*Tail, error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.path(currentName), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	t := &Tail{log: l, file: f}
	if err := t.start(ctx); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *Tail) start(ctx context.Context) error {
	st, err := t.file.Stat()
	if err != nil {
		return err
	}
	if t.seq, t.at, err = lastLine(ctx, t.file, st.Size()); err != nil || t.seq != 0 {
		return err
	}
	t.seq, err = t.log.lastRotated(ctx)
	return err
}

// Seq is the sequence number of the last event the tail delivered, or the
// one it started after.
func (t *Tail) Seq() int64 { return t.seq }

// Close releases the file the tail holds.
func (t *Tail) Close() error {
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

// read reads the file the tail holds from where it stands.
func (t *Tail) read(ctx context.Context) ([]Event, error) {
	events, at, err := readFrom(ctx, t.file, t.at)
	if err != nil {
		return nil, err
	}
	t.at = at
	return events, nil
}

// Next returns the events logged since the last call, oldest first, and
// false when it cannot vouch that they are all of them: some rotated away
// unread, the log was recreated, emptied or damaged, or it went back behind
// the tail. The caller then starts over from a snapshot and a new tail.
func (t *Tail) Next(ctx context.Context) ([]Event, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	cur, curID, err := t.log.openRead(currentName)
	if err != nil {
		return nil, false, err
	}
	held := false
	defer func() {
		if cur != nil && !held {
			cur.Close()
		}
	}()
	before, whole, same := t.seq, t.file == nil, false
	var events []Event
	if t.file != nil {
		st, err := t.file.Stat()
		if err != nil {
			return nil, false, err
		}
		if st.Size() < t.at {
			return nil, false, nil // emptied under the tail
		}
		// The rest of the file the tail holds; once rotated, then any file
		// between it and the current one.
		if events, err = t.read(ctx); err != nil {
			return nil, false, err
		}
		same = cur != nil && identity(st) == curID
		if !same {
			rotated, rotatedID, err := t.log.openRead(rotatedName)
			if err != nil {
				return nil, false, err
			}
			if rotated != nil {
				if rotatedID != identity(st) {
					var between []Event
					between, _, err = readFrom(ctx, rotated, 0)
					events = append(events, between...)
				}
				rotated.Close()
			}
			if err != nil {
				return nil, false, err
			}
		}
	} else if events, err = t.log.read(ctx, rotatedName); err != nil {
		return nil, false, err
	}
	if !same {
		t.Close()
		t.file, t.at, held = cur, 0, cur != nil
		if cur != nil {
			var more []Event
			if more, err = t.read(ctx); err != nil {
				return nil, false, err
			}
			events = append(events, more...)
		}
	}
	out, last := after1(before, events)
	complete := continues(before, out)
	if whole && before != 0 && len(out) == 0 && last != before {
		complete = false // the log holds nothing after the tail, and not the tail
	}
	if len(out) > 0 {
		t.seq = out[len(out)-1].Seq
	}
	return out, complete, nil
}

type fileID struct{ dev, ino uint64 }
