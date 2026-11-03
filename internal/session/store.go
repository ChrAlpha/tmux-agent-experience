package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
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
	sum := sha256.Sum256([]byte("session\x00" + agent + "\x00" + id))
	return lockfile.With(ctx, filepath.Join(s.Dir, "locks", "s-"+hex.EncodeToString(sum[:8])), fn)
}

// Get reads a session; ok is false when there is none.
func (s Store) Get(agent, id string) (rec Record, ok bool, err error) {
	path, err := s.path(agent, id)
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("session %s/%s: %w", agent, id, err)
	}
	return rec, true, nil
}

// Put writes a session. The caller holds its lock.
func (s Store) Put(rec Record) error {
	path, err := s.path(rec.Agent, rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Delete removes a session. The caller holds its lock.
func (s Store) Delete(agent, id string) error {
	path, err := s.path(agent, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Save stores rec and, when its state is one the event log does not report
// yet, logs ev for it, in an order that leaves no change unreported: the
// record first, marked with the change, then the event, then the record
// with the mark cleared. A writer that dies in between leaves the mark, and
// the next holder of the lock logs the change (Settle); at worst an event is
// logged twice, never not at all. The caller holds the session's lock. It
// returns the record as stored.
func (s Store) Save(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) (Record, error) {
	if !rec.Unlogged() {
		rec.Change = ""
		return rec, s.Put(rec)
	}
	rec.Change = ev.Event
	if err := s.Put(rec); err != nil {
		return rec, err
	}
	return s.logged(ctx, log, rec, ev)
}

func (s Store) logged(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) (Record, error) {
	ev.Agent, ev.Session = rec.Agent, rec.ID
	rep := rec.Reports()
	ev.State, ev.Kind, ev.Turn = rep.State, rep.Kind, rep.Turn
	if err := log.Append(ctx, &ev); err != nil {
		return rec, err
	}
	rec.Logged, rec.Change = rep, ""
	return rec, s.Put(rec)
}

// End ends the session with ev (SessionEnd or Exit): the record is marked
// ended, the event logged, the record deleted. A writer that dies in between
// leaves the marked record, which readers treat as gone and the next holder
// of the lock finishes (Settle). The caller holds the session's lock.
func (s Store) End(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) error {
	rec.Ended = ev.Event
	if err := s.Put(rec); err != nil {
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
	return s.Delete(rec.Agent, rec.ID)
}

// Settle finishes what a writer of rec did not: it logs a change left
// unlogged, or completes an end. It returns the record as it stands after,
// and false when the session is gone. The caller holds the session's lock.
func (s Store) Settle(ctx context.Context, log *eventlog.Log, rec Record) (Record, bool, error) {
	if rec.Ended != "" {
		return Record{}, false, s.ended(ctx, log, rec, eventlog.Event{})
	}
	if rec.Unlogged() {
		change := rec.Change
		if change == "" {
			change = "Unlogged"
		}
		rec, err := s.logged(ctx, log, rec, eventlog.Event{Event: change, Cwd: rec.Cwd})
		return rec, true, err
	}
	return rec, true, nil
}

// All reads every session. Unreadable files are skipped: one corrupt session
// must not hide the others.
func (s Store) All() ([]Record, error) {
	var out []Record
	for _, agent := range []string{proc.Claude, proc.Codex} {
		entries, err := os.ReadDir(filepath.Join(s.root(), agent))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidID(id) {
				continue
			}
			if rec, found, err := s.Get(agent, id); err == nil && found {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}
