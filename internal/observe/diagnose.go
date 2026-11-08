package observe

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

type SuccessEvidence struct {
	Generation uint64     `json:"generation"`
	At         *time.Time `json:"at,omitempty"`
}
type AttemptEvidence struct {
	At               *time.Time `json:"at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	PriorCompletedAt *time.Time `json:"prior_completed_at,omitempty"`
	Outcome          string     `json:"outcome"`
	FailureCode      string     `json:"failure_code,omitempty"`
	PreviousFailure  string     `json:"previous_failure,omitempty"`
}

// Diagnosis is sampled metadata evidence, not a refresh or a worker identity.
type Diagnosis struct {
	Complete    bool             `json:"complete"`
	Lock        string           `json:"lock"`
	State       string           `json:"state"`
	Image       string           `json:"image"`
	LastSuccess *SuccessEvidence `json:"last_success,omitempty"`
	LastAttempt AttemptEvidence  `json:"last_attempt"`
	Note        string           `json:"note"`
}

func timeRef(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	at := t.UTC()
	return &at
}

// Diagnose probes only an existing lock and fixed bounded files. Holding a
// successful nonblocking probe excludes a refresh while metadata is sampled;
// contention itself proves neither a worker's identity nor its health.
func Diagnose(ctx context.Context, root string) Diagnosis {
	d := Diagnosis{Lock: "unknown", State: "unknown", Image: "unknown", LastAttempt: AttemptEvidence{Outcome: "unknown"}, Note: "sampled metadata; hook contact never proves state freshness"}
	held, lockErr := lockfile.TryExisting(ctx, root, "observe/refresh-lock")
	switch {
	case lockErr == nil:
		d.Lock = "free"
		defer held.Close()
	case errors.Is(lockErr, lockfile.ErrBusy):
		d.Lock = "held"
	case errors.Is(lockErr, os.ErrNotExist):
		d.Lock = "absent"
	}
	stateBytes, stateErr := statefile.ReadKnown(ctx, root, "observe/state.json", 4096)
	var state refreshState
	if stateErr == nil {
		state, stateErr = decodeState(stateBytes)
	}
	switch {
	case stateErr == nil:
		d.State = "readable"
	case errors.Is(stateErr, os.ErrNotExist):
		d.State = "absent"
	default:
		d.State = "unreadable"
	}
	img, imageErr := readImageUsing(ctx, root, statefile.ReadKnown)
	switch {
	case imageErr == nil:
		age := time.Since(img.Created)
		switch {
		case age < 0:
			d.Image = "clock_uncertain"
		case age > time.Duration(max(2, img.Interval)*2)*time.Second:
			d.Image = "stale"
		default:
			d.Image = "current"
		}
	case errors.Is(imageErr, os.ErrNotExist):
		d.Image = "absent"
	default:
		d.Image = "unreadable"
	}
	if stateErr == nil {
		if state.Generation > 0 {
			d.LastSuccess = &SuccessEvidence{Generation: state.Generation, At: timeRef(state.SuccessAt)}
			if d.LastSuccess.At == nil && imageErr == nil && img.Generation == state.Generation {
				d.LastSuccess.At = timeRef(img.Created)
			}
		}
		d.LastAttempt.At = timeRef(state.Attempt)
		if state.InProgress {
			d.LastAttempt.PriorCompletedAt = timeRef(state.CompletedAt)
		} else {
			d.LastAttempt.CompletedAt = timeRef(state.CompletedAt)
		}
		switch {
		case state.InProgress:
			switch d.Lock {
			case "held":
				d.LastAttempt.Outcome = "active_or_contended"
			case "free":
				d.LastAttempt.Outcome = "incomplete"
			default:
				d.LastAttempt.Outcome = "incomplete_or_unknown"
			}
			if state.Failure {
				d.LastAttempt.PreviousFailure = state.FailureCode
			}
		case state.Failure:
			d.LastAttempt.Outcome = "completed_failure"
			d.LastAttempt.FailureCode = state.FailureCode
			if d.LastAttempt.FailureCode == "" {
				d.LastAttempt.FailureCode = "observation_unavailable"
			}
		case state.Generation > 0:
			d.LastAttempt.Outcome = "succeeded"
		case state.Attempt.IsZero():
			d.LastAttempt.Outcome = "never_observed"
		default:
			d.LastAttempt.Outcome = "unknown"
		}
		if imageErr == nil && img.Generation != state.Generation {
			d.Image = "uncommitted_or_inconsistent"
		}
	} else if errors.Is(stateErr, os.ErrNotExist) && errors.Is(imageErr, os.ErrNotExist) && d.Lock != "held" {
		d.LastAttempt.Outcome = "never_observed"
	}
	d.Complete = ctx.Err() == nil && d.State != "unreadable" && d.Image != "unreadable" && d.Image != "uncommitted_or_inconsistent" && d.Lock != "unknown"
	if d.Lock == "held" {
		d.Note += "; lock held at probe; metadata is not an atomic worker-liveness proof"
	}
	return d
}
