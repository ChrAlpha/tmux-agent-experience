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

// GetContext reads bounded session metadata and observes the operation budget.
func (s Store) GetContext(ctx context.Context, agent, id string) (rec Record, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return Record{}, false, err
	}
	data, err := contextio.ReadFile(ctx, path, 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("session %s/%s: %w", agent, id, err)
	}
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// AllContext reads in batches. A canceled enumeration returns no partial view;
// callers must never reconcile absence from an incomplete read.
func (s Store) AllContext(ctx context.Context) ([]Record, error) {
	var out []Record
	for _, agent := range []string{proc.Claude, proc.Codex} {
		err := contextio.Entries(ctx, filepath.Join(s.root(), agent), func(e os.DirEntry) error {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidID(id) {
				return nil
			}
			if rec, found, err := s.GetContext(ctx, agent, id); err == nil && found {
				out = append(out, rec)
			}
			return ctx.Err()
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
