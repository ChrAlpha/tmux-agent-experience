package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// MaxRecordBytes bounds how much of a session file a reader reads. What a
// record holds is bounded far below it, so a larger file is damage.
const MaxRecordBytes = 4 << 20

// RecordError names a session file tmag cannot read, and why, without what
// it holds.
type RecordError struct{ Agent, ID, Category string }

func (e *RecordError) Error() string {
	location := e.Agent
	if e.ID != "" {
		location += "/" + e.ID
	}
	if location == "" {
		location = "sessions"
	}
	return fmt.Sprintf("records_unavailable: %s (%s)", location, e.Category)
}

// Get reads a session's record: ok is false when there is none, and the
// error a *RecordError when its file cannot be read. Cancellation remains the
// caller's context error.
func (s Store) Get(ctx context.Context, agent, id string) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return Record{}, false, err
	}
	fault := func(category string) (Record, bool, error) { return Record{}, false, &RecordError{agent, id, category} }
	data, err := contextio.ReadFile(ctx, path, MaxRecordBytes)
	switch {
	case ctx.Err() != nil:
		return Record{}, false, ctx.Err()
	case errors.Is(err, os.ErrNotExist):
		return Record{}, false, nil
	case errors.Is(err, os.ErrPermission):
		return fault("permission")
	case err != nil:
		return fault("unreadable")
	}
	var rec Record
	if json.Unmarshal(data, &rec) != nil {
		return fault("decode")
	}
	if rec.Agent != agent || rec.ID != id || rec.Host.PID < 0 {
		return fault("identity")
	}
	switch rec.Main {
	case "", "idle", "working", "unknown":
	default:
		return fault("state")
	}
	return rec, true, nil
}

// All returns the sessions whose records can be read, and the records that
// cannot. Views and reconciliation leave an unreadable one out, as though it
// were gone, and doctor reports it; the session's next hook writes it anew.
// The error is one of enumeration or cancellation.
func (s Store) All(ctx context.Context) ([]Record, []*RecordError, error) {
	var recs []Record
	var faults []*RecordError
	for _, agent := range []string{proc.Claude, proc.Codex} {
		err := contextio.Entries(ctx, filepath.Join(s.root(), agent), func(e os.DirEntry) error {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidID(id) {
				return nil
			}
			rec, found, err := s.Get(ctx, agent, id)
			var fault *RecordError
			switch {
			case errors.As(err, &fault):
				faults = append(faults, fault)
			case err != nil:
				return err
			case found:
				recs = append(recs, rec)
			}
			return nil
		})
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("read sessions of %s: %w", agent, err)
		}
	}
	return recs, faults, nil
}
