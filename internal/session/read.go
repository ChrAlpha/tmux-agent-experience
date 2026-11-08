package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// RecordIssue contains only a validated metadata reference and fixed category.
type RecordIssue struct {
	Agent    string `json:"agent"`
	ID       string `json:"id"`
	Category string `json:"category"`
}

func (e *RecordIssue) Error() string {
	return fmt.Sprintf("records_unavailable: %s/%s (%s)", e.Agent, e.ID, e.Category)
}

// Catalog preserves individually valid records without claiming a complete image.
type Catalog struct {
	Records    []Record
	Issues     []RecordIssue
	IssueCount uint64
	Complete   bool
}

// RecordsUnavailable refuses a complete v1 image that could fabricate absence.
type RecordsUnavailable struct{ Catalog Catalog }

func (*RecordsUnavailable) Error() string {
	return "records_unavailable: session metadata is incomplete or unreadable; run tmag doctor"
}

func (c *Catalog) issue(i RecordIssue) {
	if c.IssueCount < math.MaxUint64 {
		c.IssueCount++
	}
	if len(c.Issues) < 16 {
		c.Issues = append(c.Issues, i)
	}
}

// GetContext returns false only on actual absence. Decoder text never escapes.
func (s Store) GetContext(ctx context.Context, agent, id string) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	path, err := s.path(agent, id)
	if err != nil {
		return Record{}, false, err
	}
	issue := func(category string) (Record, bool, error) { return Record{}, false, &RecordIssue{agent, id, category} }
	// Open once. Ownership, type, size and bounded bytes refer to this exact
	// descriptor, including when an atomic publication replaces the pathname.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if ctx.Err() != nil {
		if f != nil {
			_ = f.Close()
		}
		return Record{}, false, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return issue("permission")
		}
		if errors.Is(err, syscall.ELOOP) {
			return issue("unsafe_object")
		}
		return issue("io")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return issue("io")
	}
	if !st.Mode().IsRegular() || !ownedfs.Owner(st) {
		return issue("unsafe_object")
	}
	if st.Size() > 4<<20 {
		return issue("too_large")
	}
	data, err := contextio.ReadAll(ctx, f, 4<<20)
	if ctx.Err() != nil {
		return Record{}, false, ctx.Err()
	}
	if len(data) > 4<<20 {
		return issue("too_large")
	}
	if err != nil {
		return issue("io")
	}

	var rec Record
	if json.Unmarshal(data, &rec) != nil {
		return issue("decode")
	}
	if rec.Agent != agent || rec.ID != id || !ValidRecord(rec) {
		return issue("identity")
	}
	return rec, true, nil
}

// ValidRecord validates the metadata domain, including legacy optional fields.
func ValidRecord(r Record) bool {
	if !validAgent(r.Agent) || !ValidID(r.ID) || r.Host.PID < 0 {
		return false
	}
	if r.Host.Role != "" && r.Host.Role != proc.Terminal && r.Host.Role != proc.Service {
		return false
	}
	switch r.Main {
	case "", "unknown", "idle", "working":
	default:
		return false
	}
	if r.Ended != "" && r.Ended != "Exit" && r.Ended != "SessionEnd" {
		return false
	}
	if r.Observation != nil {
		switch r.Observation.Host {
		case "unknown", "verified", "unavailable":
		default:
			return false
		}
	}
	return true
}

// ScanContext exposes faults while retaining valid records for non-destructive repair.
func (s Store) ScanContext(ctx context.Context) (Catalog, error) { return s.ScanBoundedContext(ctx, 0) }

// ErrCatalogCapacity is an explicit collection limit, never a partial image.
var ErrCatalogCapacity = errors.New("session catalog capacity exceeded")

// ScanBoundedContext also caps encoded metadata retained for a shared image.
// Zero preserves ordinary complete collection semantics.
func (s Store) ScanBoundedContext(ctx context.Context, limit int) (Catalog, error) {
	return s.scanContext(ctx, limit, 0)
}

// ProbeContext limits physical directory entries as well as retained bytes.
// It is read-only and returns explicit incomplete evidence at either bound.
func (s Store) ProbeContext(ctx context.Context, entries, bytes int) (Catalog, error) {
	if entries < 1 || bytes < 1 {
		return Catalog{}, ErrCatalogCapacity
	}
	return s.scanContext(ctx, bytes, entries)
}
func (s Store) scanContext(ctx context.Context, limit, maxEntries int) (Catalog, error) {
	examined := 0
	used := 0
	c := Catalog{Complete: true}
	for _, agent := range []string{proc.Claude, proc.Codex} {
		err := contextio.Entries(ctx, filepath.Join(s.root(), agent), func(e os.DirEntry) error {
			examined++
			if maxEntries > 0 && examined > maxEntries {
				return ErrCatalogCapacity
			}
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !ValidID(id) {
				return nil
			}
			rec, found, err := s.GetContext(ctx, agent, id)
			if err != nil {
				var issue *RecordIssue
				if errors.As(err, &issue) {
					c.issue(*issue)
				} else {
					return err
				}
			} else if found {
				if limit > 0 {
					data, err := json.Marshal(rec)
					if err != nil {
						return err
					}
					used += len(data) + 1
					if used > limit {
						return ErrCatalogCapacity
					}
				}
				c.Records = append(c.Records, rec)
			}
			return ctx.Err()
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			c.Complete = false
			if !errors.Is(err, ErrCatalogCapacity) && ctx.Err() == nil {
				c.issue(RecordIssue{Agent: agent, Category: "scan_incomplete"})
			}
			return c, err
		}
	}
	return c, ctx.Err()
}

// AllContext returns a truthful complete collection or an explicit error.
func (s Store) AllContext(ctx context.Context) ([]Record, error) {
	c, err := s.ScanContext(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || !c.Complete || c.IssueCount != 0 {
		return nil, &RecordsUnavailable{Catalog: c}
	}
	return c.Records, nil
}
