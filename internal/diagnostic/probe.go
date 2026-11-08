package diagnostic

import (
	"context"
	"errors"
	"math"
	"os"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

type BucketView struct {
	Component      string    `json:"component"`
	Operation      string    `json:"operation"`
	Category       string    `json:"category"`
	Count          uint64    `json:"count"`
	First          time.Time `json:"first"`
	Last           time.Time `json:"last"`
	ClockRollbacks uint64    `json:"clock_rollbacks,omitempty"`
	Agent          string    `json:"agent,omitempty"`
	ID             string    `json:"id,omitempty"`
}
type History struct {
	State          string       `json:"state"`
	BestEffort     bool         `json:"best_effort"`
	RecordedTotal  uint64       `json:"recorded_total"`
	TotalSaturated bool         `json:"total_saturated"`
	BucketsTotal   int          `json:"buckets_total"`
	Overflow       uint64       `json:"unbucketed_occurrences"`
	Truncated      bool         `json:"truncated"`
	Buckets        []BucketView `json:"buckets"`
}

// Probe never writes metadata. Historical counts are not current failures.
func Probe(ctx context.Context, root string, limit int) History {
	h := History{State: "unknown", BestEffort: true, Buckets: []BucketView{}}
	if limit < 1 || limit > MaxBuckets {
		return h
	}
	data, err := statefile.ReadKnown(ctx, root, "diagnostics/summary.json", MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		h.State = "never_recorded"
		return h
	}
	if err != nil {
		h.State = "unreadable"
		return h
	}
	s, err := decodeSummary(ctx, data)
	if err != nil {
		h.State = "unreadable"
		return h
	}
	h.State = "readable"
	h.BucketsTotal = len(s.Buckets)
	h.Overflow = s.Overflow
	h.RecordedTotal = s.Overflow
	h.TotalSaturated = s.Overflow == math.MaxUint64
	for _, b := range s.Buckets {
		h.TotalSaturated = h.TotalSaturated || b.Count == math.MaxUint64
		if math.MaxUint64-h.RecordedTotal < b.Count {
			h.RecordedTotal = math.MaxUint64
			h.TotalSaturated = true
		} else {
			h.RecordedTotal += b.Count
		}
		if len(h.Buckets) < limit {
			h.Buckets = append(h.Buckets, BucketView{Component: b.Component, Operation: b.Operation, Category: b.Category, Count: b.Count, First: b.First, Last: b.Last, ClockRollbacks: b.ClockRollbacks, Agent: b.Agent, ID: b.ID})
		}
	}
	h.Truncated = len(s.Buckets) > len(h.Buckets) || h.Overflow > 0
	return h
}
