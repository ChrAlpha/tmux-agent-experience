// Package diagnostic records bounded category metadata, never error strings,
// payloads, command output or dynamic bucket keys. Counts are best effort.
package diagnostic

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const MaxBytes = 32 << 10
const MaxBuckets = 32
const MaxLogBytes = 256 << 10

type Fault struct{ Component, Operation, Category, Agent, ID string }
type Bucket struct {
	Fault
	Count          uint64
	First, Last    time.Time
	ClockRollbacks uint64
}
type Summary struct {
	Version  int
	Buckets  []Bucket
	Overflow uint64
}

func allowed(s string, values ...string) bool { return slices.Contains(values, s) }
func valid(f Fault) bool {
	return allowed(f.Component, "hook", "reconcile", "maintenance", "notify", "observe", "admission") &&
		allowed(f.Operation, "run", "scan", "session", "pane", "suppress", "settle_pane", "refresh", "acquire", "execute") &&
		allowed(f.Category, "io", "decode", "identity", "too_large", "permission", "unsafe_object", "scan_incomplete", "deferred", "busy", "resource_limit", "timeout", "failed", "unavailable", "invalid_option", "history_unavailable") &&
		(f.Agent == "" || allowed(f.Agent, "claude", "codex", "notify")) && (f.ID == "" || session.ValidID(f.ID))
}
func saturate(n *uint64) {
	if *n < math.MaxUint64 {
		*n++
	}
}

func Read(ctx context.Context, root string) (Summary, error) {
	var s Summary
	data, err := statefile.ReadSmallWithin(ctx, root, "diagnostics/summary.json", MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Summary{Version: 1}, nil
	}
	if err != nil {
		return s, err
	}
	return decodeSummary(ctx, data)
}
func decodeSummary(ctx context.Context, data []byte) (Summary, error) {
	var s Summary
	if json.Unmarshal(data, &s) != nil || s.Version != 1 || len(s.Buckets) > MaxBuckets {
		return Summary{}, errors.New("diagnostic_unavailable")
	}
	seen := map[[3]string]bool{}
	for _, b := range s.Buckets {
		key := [3]string{b.Component, b.Operation, b.Category}
		if seen[key] || !valid(b.Fault) || b.Count == 0 || b.First.IsZero() || b.Last.IsZero() || b.First.After(b.Last) {
			return Summary{}, errors.New("diagnostic_unavailable")
		}
		seen[key] = true
	}
	return s, ctx.Err()
}

// Record receives resolved historical recording consent from its caller; it
// performs no discovery and cannot recursively diagnose its own failure.
func Record(ctx context.Context, root string, f Fault) error {
	if !valid(f) {
		return errors.New("invalid diagnostic category")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	return lockfile.Within(ctx, root, "diagnostics/lock", false, false, func() error {
		s, err := Read(ctx, root)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		index := -1
		for i, b := range s.Buckets {
			if b.Component == f.Component && b.Operation == f.Operation && b.Category == f.Category {
				index = i
				break
			}
		}
		if index < 0 && len(s.Buckets) < MaxBuckets {
			s.Buckets = append(s.Buckets, Bucket{Fault: f, First: now})
			index = len(s.Buckets) - 1
		}
		if index < 0 {
			saturate(&s.Overflow)
		} else {
			b := &s.Buckets[index]
			b.Fault = f
			// First/Last span observed wall-clock values, not operation order.
			// A rollback widens that range and is recorded explicitly.
			if now.Before(b.Last) {
				saturate(&b.ClockRollbacks)
			}
			if now.Before(b.First) {
				b.First = now
			}
			if now.After(b.Last) {
				b.Last = now
			}
			saturate(&b.Count)
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if len(data) > MaxBytes {
			return errors.New("diagnostic_capacity")
		}
		return statefile.WriteSlots(ctx, root, "diagnostics/summary.json", "diagnostics/.summary-00", ".summary-", 1, data)
	})
}

// AppendLog serializes size-check, rotation and append. Older raw records are
// preserved until normal bounded rotation, and never copied to new artifacts.
func AppendLog(ctx context.Context, root string, f Fault) error {
	if !valid(f) {
		return errors.New("invalid diagnostic category")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	return lockfile.Within(ctx, root, "diagnostics/log-lock", false, false, func() error {
		d, err := ownedfs.Open(ctx, root, ".", true)
		if err != nil {
			return err
		}
		defer d.Close()
		file, err := d.OpenFile(ctx, "hook.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := json.Marshal(struct {
			Time time.Time
			Fault
		}{time.Now().UTC(), f})
		if err != nil {
			return err
		}
		data = append(data, '\n')
		st, err := file.Stat()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if st.Size()+int64(len(data)) > MaxLogBytes {
			if err := file.Truncate(0); err != nil {
				return err
			}
		}
		_, err = file.Write(data)
		return err
	})
}
