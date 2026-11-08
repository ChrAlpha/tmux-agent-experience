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
	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

// RecordError identifies the first unreadable metadata location without decoder
// output or file contents. Complete views must not treat it as absence.
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

// GetContext reads one bounded, owned regular record. Only definite absence is
// false/nil; cancellation remains the caller's context error.
func (s Store) GetContext(ctx context.Context, agent, id string) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return Record{}, false, err
	}
	fault := func(category string) (Record, bool, error) { return Record{}, false, &RecordError{agent, id, category} }
	if _, err := coord.Current(ctx, s.Dir); err != nil {
		return Record{}, false, err
	}
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil {
		return Record{}, false, err
	}
	data, err := statefile.ReadSmallWithin(ctx, s.Dir, rel, 4<<20)
	if ctx.Err() != nil {
		return Record{}, false, ctx.Err()
	}
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if errors.Is(err, ownedfs.ErrScope) {
		return fault("unsafe_object")
	}
	if errors.Is(err, os.ErrPermission) {
		return fault("permission")
	}
	if err != nil {
		return fault("unreadable_or_oversized")
	}
	var rec Record
	if json.Unmarshal(data, &rec) != nil {
		return fault("decode")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	if rec.Agent != agent || rec.ID != id || rec.Host.PID < 0 {
		return fault("identity")
	}
	if rec.Ended != "" && rec.Ended != "Exit" && rec.Ended != "SessionEnd" {
		return fault("state")
	}
	switch rec.Main {
	case "", "idle", "working", "unknown":
	default:
		return fault("state")
	}
	return rec, true, nil
}

// AllContext returns no partial view. The first exact fault stops enumeration;
// repair never infers unique attachments or absence from damaged metadata.
func (s Store) AllContext(ctx context.Context) ([]Record, error) {
	if _, err := coord.Current(ctx, s.Dir); err != nil {
		return nil, err
	}
	var out []Record
	for _, dir := range []string{s.Dir, s.root()} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		if err != nil {
			return nil, &RecordError{Category: "enumeration"}
		}
		if !st.IsDir() || !ownedfs.Owner(st) {
			return nil, &RecordError{Category: "unsafe_object"}
		}
	}
	for _, agent := range []string{proc.Claude, proc.Codex} {
		dir := filepath.Join(s.root(), agent)
		if st, err := os.Lstat(dir); err == nil && (!st.IsDir() || !ownedfs.Owner(st)) {
			return nil, &RecordError{Agent: agent, Category: "unsafe_object"}
		}
		err := contextio.Entries(ctx, dir, func(e os.DirEntry) error {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidID(id) {
				return nil
			}
			rec, found, err := s.GetContext(ctx, agent, id)
			if err != nil {
				return err
			}
			if found {
				out = append(out, rec)
			}
			return ctx.Err()
		})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			var record *RecordError
			if errors.As(err, &record) {
				return nil, err
			}
			return nil, &RecordError{Agent: agent, Category: "enumeration"}
		}
	}
	return out, ctx.Err()
}
