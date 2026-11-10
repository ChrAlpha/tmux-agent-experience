package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// Store keeps one file per live session under <state dir>/sessions/<agent>/.
// Writers hold the session's lock; each write replaces the file whole, so
// readers need none.
type Store struct {
	Dir string // the state directory
}

// idRE bounds what an agent may use as a session id, which becomes a file
// name: every agent tmag knows uses UUIDs.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID reports whether id can name a session.
func ValidID(id string) bool { return idRE.MatchString(id) }

func (s Store) root() string { return filepath.Join(s.Dir, "sessions") }

func (s Store) path(agent, id string) (string, error) {
	if !proc.IsAgent(agent) || !ValidID(id) {
		return "", fmt.Errorf("invalid session %s/%q", agent, id)
	}
	return filepath.Join(s.root(), agent, id+".json"), nil
}

// Lock runs fn holding the session's lock, a stripe of the state directory's
// lock pool. Every change to a session's file happens under it, read and
// written in one go, and so is the event reporting the change, so that a
// session's events are logged in the order its changes were made.
func (s Store) Lock(ctx context.Context, agent, id string, fn func() error) error {
	return lockfile.Stripe(ctx, s.Dir, "s", sessionKey(agent, id), fn)
}

func sessionKey(agent, id string) [32]byte {
	return sha256.Sum256([]byte("session\x00" + agent + "\x00" + id))
}

// Put stores rec as its session's record. The caller holds its lock.
func (s Store) Put(ctx context.Context, rec Record) error {
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The session's stripe owns this temporary: whoever writes holds the
	// stripe's lock, so one temporary serves every session of the stripe, and
	// a writer that dies leaves at most it behind, for the next to reuse.
	tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%02x", sessionKey(rec.Agent, rec.ID)[0]))
	f, err := contextio.OpenRegular(ctx, tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = contextio.WriteAll(ctx, f, data)
	if closed := f.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Delete removes a session's record. The caller holds its lock.
func (s Store) Delete(ctx context.Context, agent, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Save stores rec, which a hook made of prev, and logs ev reporting it when
// what an event reports of the session changed: tool calls or contact time
// alone are no event. The record goes first, so that a writer that dies
// before logging leaves the change in the record, where the session's next
// event and the next snapshot report it. The caller holds the session's lock.
func (s Store) Save(ctx context.Context, log *eventlog.Log, prev, rec Record, ev eventlog.Event) error {
	if err := s.Put(ctx, rec); err != nil {
		return err
	}
	if rec.Reports() == prev.Reports() {
		return nil
	}
	rec.Report(&ev)
	return log.Append(ctx, &ev)
}

// Report stores rec and logs ev reporting it: ev is a delivery of tmag's (a
// prompt, an interrupt, an answer) and rec what it made of the session,
// which the agent may not report itself. ev receives the event as logged,
// with its sequence number. The caller holds the session's lock.
func (s Store) Report(ctx context.Context, log *eventlog.Log, rec Record, ev *eventlog.Event) error {
	if err := s.Put(ctx, rec); err != nil {
		return err
	}
	rec.Report(ev)
	return log.Append(ctx, ev)
}

// End ends the session with ev (SessionEnd or Exit): its record is deleted,
// then the event logged. The caller holds the session's lock.
func (s Store) End(ctx context.Context, log *eventlog.Log, rec Record, ev eventlog.Event) error {
	if err := s.Delete(ctx, rec.Agent, rec.ID); err != nil {
		return err
	}
	ev.Agent, ev.Session = rec.Agent, rec.ID
	if ev.Cwd == "" {
		ev.Cwd = rec.Cwd
	}
	if ev.LastHookAt.IsZero() {
		ev.LastHookAt = rec.LastHookAt
	}
	return log.Append(ctx, &ev)
}
