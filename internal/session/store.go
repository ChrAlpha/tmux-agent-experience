package session

import (
	"context"
	"crypto/sha256"
	"errors"
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
func (s Store) Get(agent, id string) (Record, bool, error) {
	return s.GetContext(context.Background(), agent, id)
}

// Put is a compatibility helper for fixtures. The caller holds its lock.
// Production mutations pass their operation context to PutContext.
func (s Store) Put(rec Record) error {
	return s.PutContext(context.Background(), rec)
}

// PutContext publishes a session under its lock, refused with a *LimitError
// when it encodes to more than MaxRecordBytes. A change the event log reports
// goes through Save, Report or End instead.
func (s Store) PutContext(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.path(rec.Agent, rec.ID); err != nil {
		return err
	}
	data, err := readers.marshal(partRecord, rec)
	if err != nil {
		return err
	}
	return s.write(ctx, rec.Agent, rec.ID, data)
}

// write publishes a session's encoded record. One fixed temp per agent and
// stripe replaces per-invocation random files. The temp shares the target's
// parent filesystem. The publisher holds its session stripe and reuses any
// leftover at that reserved position.
func (s Store) write(ctx context.Context, agent, id string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return err
	}
	if _, err := coord.Current(ctx, s.Dir); err != nil {
		return err
	}
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil {
		return err
	}
	tmp, err := filepath.Rel(s.Dir, s.temporary(agent, coord.Slot(sessionKey(agent, id))))
	if err != nil {
		return err
	}
	return statefile.WriteWithin(ctx, s.Dir, rel, tmp, data)
}

// Delete removes a session. The caller holds its lock.
func (s Store) Delete(agent, id string) error {
	return s.DeleteContext(context.Background(), agent, id)
}

// DeleteContext removes only a record under the selected, runtime-pinned realm.
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
// yet, logs change for it: an event of that name reporting the session, and
// nothing more, as Settle logs it for a writer that died. A delivery of
// tmag's, whose event carries more, is logged with Report. The order leaves
// no change unreported: the record first, marked with the change, then the
// event, then the record with the mark cleared. A writer that dies in
// between leaves the mark, and the next holder of the lock logs the change
// (Settle); at worst an event is logged twice, never not at all. Every
// publication is admitted first (PrepareSave): a change too large to record
// is refused with a *LimitError before any. The caller holds the session's
// lock. It returns the record as stored.
func (s Store) Save(ctx context.Context, log *eventlog.Log, rec Record, change string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	p, err := PrepareSave(rec, change)
	if err != nil {
		return rec, err
	}
	rec, _, err = s.Publish(ctx, log, p)
	return rec, err
}

// Report stores rec and logs ev reporting it, whether or not what it
// reports changed: ev is a delivery of tmag's (an interrupt, an answer) and
// rec what it did to the session, which the agent does not report itself.
// Like Save, it marks the change before logging it, and keeps ev with the
// mark, so that the next holder of the lock logs ev as it was (Settle), and
// it admits every publication first (PrepareReport). ev receives the event
// as logged, with its sequence number. The caller holds the session's lock.
func (s Store) Report(ctx context.Context, log *eventlog.Log, rec Record, ev *eventlog.Event) (Record, error) {
	if err := ctx.Err(); err != nil {
		return rec, err
	}
	p, err := PrepareReport(rec, *ev)
	if err != nil {
		return rec, err
	}
	rec, logged, err := s.Publish(ctx, log, p)
	if err == nil {
		*ev = logged
	}
	return rec, err
}

// End ends the session with ev (SessionEnd or Exit): the session is marked
// ended, the event logged, the record deleted. A writer that dies in between
// leaves the ended record, which readers treat as gone and the next holder
// of the lock finishes (Settle). The ended record keeps only what the event
// reports (PrepareEnd), so a session ends whatever size its record reached.
// The caller holds the session's lock.
func (s Store) End(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := PrepareEnd(rec, ev)
	if err != nil {
		return err
	}
	_, _, err = s.Publish(ctx, log, p)
	return err
}

// Publish makes a prepared change, in the order that leaves no change
// unreported: the marked record, the event, then the final record, or the
// deletion that ends the session. It writes exactly the bytes admitted. It
// returns the record as stored and the event as logged. The caller holds
// the session's lock, as it has since preparing the change.
func (s Store) Publish(ctx context.Context, log *eventlog.Log, p Prepared) (Record, eventlog.Event, error) {
	if p.agent == "" {
		return Record{}, eventlog.Event{}, errors.New("publishing a change that was not prepared")
	}
	if p.marked != nil {
		if err := s.write(ctx, p.agent, p.id, p.marked); err != nil {
			return Record{}, eventlog.Event{}, err
		}
	}
	var ev eventlog.Event
	if p.event != nil {
		ev = *p.event
		if err := log.Append(ctx, &ev); err != nil {
			return Record{}, eventlog.Event{}, err
		}
	}
	if p.end {
		return Record{}, ev, s.DeleteContext(ctx, p.agent, p.id)
	}
	if err := s.write(ctx, p.agent, p.id, p.final); err != nil {
		return Record{}, ev, err
	}
	return p.rec, ev, nil
}

// Settle finishes what a writer of rec did not: it logs a change left
// unlogged, a delivery of tmag's whole, or completes an end. A prompt that
// an older tmag left without a request gets one, and is logged with it. It
// returns the record as it stands after, and false when the session is gone.
// What it publishes derives from rec as a prepared change derives it, so a
// change this tmag marked always settles; a record an older tmag left whose
// repair would exceed a limit is unrepairable metadata (RecordError), left
// as it is, and nothing is logged for it. The caller holds the session's lock.
func (s Store) Settle(ctx context.Context, log *eventlog.Log, rec Record) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	if rec.Ended != "" {
		end := endEvent(rec)
		if err := readers.admit(end); err != nil {
			return Record{}, false, unrepairable(rec, err)
		}
		_, _, err := s.Publish(ctx, log, Prepared{agent: rec.Agent, id: rec.ID, event: &end, end: true})
		return Record{}, false, err
	}
	rec.name(time.Now())
	if (rec.Delivery == nil || rec.Delivery.Event != rec.Change) && !rec.Unlogged() {
		return rec, true, nil
	}
	p, err := readers.logged(rec)
	if err != nil {
		return rec, true, unrepairable(rec, err)
	}
	rec, _, err = s.Publish(ctx, log, p)
	return rec, true, err
}

func unrepairable(rec Record, err error) error {
	var limit *LimitError
	if errors.As(err, &limit) {
		return &RecordError{Agent: rec.Agent, ID: rec.ID, Category: "repair_exceeds_limit"}
	}
	return err
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

// All returns a complete collection or the first explicit metadata fault.
func (s Store) All() ([]Record, error) { return s.AllContext(context.Background()) }
