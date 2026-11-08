package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

// Store keeps one file per live session under <state dir>/sessions/<agent>/.
// Writers hold the session's lock; files are replaced atomically, so readers
// need none.
type Store struct {
	Dir string // the state directory
}

// idRE bounds what an agent may use as a session id, which becomes a file
// name: Claude Code and Codex use UUIDs.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID reports whether id can name a session.
func ValidID(id string) bool { return idRE.MatchString(id) }

func validAgent(agent string) bool { return agent == proc.Claude || agent == proc.Codex }

func (s Store) root() string { return filepath.Join(s.Dir, "sessions") }

func (s Store) path(agent, id string) (string, error) {
	if !validAgent(agent) || !ValidID(id) {
		return "", fmt.Errorf("invalid session %s/%q", agent, id)
	}
	return filepath.Join(s.root(), agent, id+".json"), nil
}

// Lock runs fn holding the session's lock. Every change to a session's file
// happens under it, read and written in one go.
func (s Store) Lock(ctx context.Context, agent, id string, fn func() error) error {
	return coord.With(ctx, s.Dir, coord.Session, sessionKey(agent, id), fn)
}

func sessionKey(agent, id string) [32]byte {
	return sha256.Sum256([]byte("session\x00" + agent + "\x00" + id))
}

func (s Store) temporary(agent string, slot byte) string {
	return filepath.Join(s.root(), agent, fmt.Sprintf(".tmag-tmp-s-%02x", slot))
}

// Get reads a session; ok is false when there is none.
func (s Store) Get(agent, id string) (rec Record, ok bool, err error) {
	return s.GetContext(context.Background(), agent, id)
}

// Put is a compatibility helper for fixtures. The caller holds its lock.
// Production mutations pass their operation context to PutContext.
func (s Store) Put(rec Record) error {
	return s.PutContext(context.Background(), rec)
}

// PutContext publishes a session under its lock. One fixed temp per agent and
// stripe replaces per-invocation random files. The temp shares the target's
// parent filesystem, and both publisher and cleaner hold the same stripe.
func (s Store) PutContext(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(rec.Agent, rec.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil {
		return err
	}
	tmp, err := filepath.Rel(s.Dir, s.temporary(rec.Agent, coord.Slot(sessionKey(rec.Agent, rec.ID))))
	if err != nil {
		return err
	}
	return statefile.WriteSlots(ctx, s.Dir, rel, tmp, ".tmag-tmp-s-", coord.Slots, data)
}

// Delete removes a session. The caller holds its lock.
func (s Store) Delete(agent, id string) error {
	return s.DeleteContext(context.Background(), agent, id)
}

// DeleteContext removes only a record under the selected, inode-bound realm.
func (s Store) DeleteContext(ctx context.Context, agent, id string) error {
	path, err := s.path(agent, id)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil {
		return err
	}
	_, err = statefile.RemoveWithin(ctx, s.Dir, rel)
	return err
}

// Save stores rec and, when its state is one the event log does not report
// yet, logs ev for it, in an order that leaves no change unreported: the
// record first, marked with the change, then the event, then the record
// with the mark cleared. A writer that dies in between leaves the mark, and
// the next holder of the lock logs the change (Settle); at worst an event is
// logged twice, never not at all. The caller holds the session's lock. It
// returns the record as stored.
func (s Store) Save(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) (Record, error) {
	rec.Delivery = nil
	if !rec.Unlogged() {
		rec.Change = ""
		return rec, s.PutContext(ctx, rec)
	}
	rec.Change = ev.Event
	if err := s.PutContext(ctx, rec); err != nil {
		return rec, err
	}
	return s.logged(ctx, log, rec, ev)
}

func (s Store) logged(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) (Record, error) {
	rec.Report(&ev)
	if err := log.Append(ctx, &ev); err != nil {
		return rec, err
	}
	rec.Logged, rec.Change, rec.Delivery = rec.Reports(), "", nil
	return rec, s.PutContext(ctx, rec)
}

// Report stores rec and logs ev reporting it, whether or not what it
// reports changed: ev is a delivery of tmag's (an interrupt, an answer) and
// rec what it did to the session, which the agent does not report itself.
// Like Save, it marks the change before logging it, and keeps ev with the
// mark, so that the next holder of the lock logs ev as it was (Settle). ev
// receives its sequence number. The caller holds the session's lock.
func (s Store) Report(ctx context.Context, log *eventlog.Log, rec Record, ev *eventlog.Event) (Record, error) {
	kept := *ev
	rec.Change, rec.Delivery = ev.Event, &kept
	if err := s.PutContext(ctx, rec); err != nil {
		return rec, err
	}
	rec.Report(ev)
	if err := log.Append(ctx, ev); err != nil {
		return rec, err
	}
	rec.Logged, rec.Change, rec.Delivery = rec.Reports(), "", nil
	return rec, s.PutContext(ctx, rec)
}

// End ends the session with ev (SessionEnd or Exit): the record is marked
// ended, the event logged, the record deleted. A writer that dies in between
// leaves the marked record, which readers treat as gone and the next holder
// of the lock finishes (Settle). The caller holds the session's lock.
func (s Store) End(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) error {
	rec.Ended = ev.Event
	if err := s.PutContext(ctx, rec); err != nil {
		return err
	}
	return s.ended(ctx, log, rec, ev)
}

func (s Store) ended(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) error {
	ev.Agent, ev.Session, ev.Event = rec.Agent, rec.ID, rec.Ended
	if ev.Cwd == "" {
		ev.Cwd = rec.Cwd
	}
	if err := log.Append(ctx, &ev); err != nil {
		return err
	}
	return s.DeleteContext(ctx, rec.Agent, rec.ID)
}

// Settle finishes what a writer of rec did not: it logs a change left
// unlogged, a delivery of tmag's whole, or completes an end. A prompt that
// an older tmag left without a request gets one, and is logged with it. It
// returns the record as it stands after, and false when the session is gone.
// The caller holds the session's lock.
func (s Store) Settle(ctx context.Context, log *eventlog.Log, rec Record) (Record, bool, error) {
	if rec.Ended != "" {
		return Record{}, false, s.ended(ctx, log, rec, eventlog.Event{})
	}
	rec.name(time.Now())
	if rec.Delivery != nil && rec.Delivery.Event == rec.Change {
		rec, err := s.logged(ctx, log, rec, *rec.Delivery)
		return rec, true, err
	}
	if rec.Unlogged() {
		change := rec.Change
		if change == "" {
			change = "Unlogged"
		}
		rec, err := s.logged(ctx, log, rec, eventlog.Event{Event: change})
		return rec, true, err
	}
	return rec, true, nil
}

// Current reads a session and settles it (Settle), so that a writer starts
// from a record the log reports and nothing a writer that died left
// unlogged is overwritten; ok is false when there is none. The caller holds
// the session's lock.
func (s Store) Current(ctx context.Context, log *eventlog.Log, agent, id string) (Record, bool, error) {
	rec, ok, err := s.GetContext(ctx, agent, id)
	if err != nil || !ok {
		return rec, false, err
	}
	return s.Settle(ctx, log, rec)
}

// All reads every session using the same strict validation as AllContext.
func (s Store) All() ([]Record, error) { return s.AllContext(context.Background()) }
