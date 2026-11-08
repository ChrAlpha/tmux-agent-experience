package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const statusLimit = 8 << 10

type Work struct {
	Examined  uint64 `json:"examined"`
	Removed   uint64 `json:"removed"`
	Deferrals uint64 `json:"deferrals"`
	Classes   uint64 `json:"classes"`
}
type Pass struct {
	ID      uint64    `json:"id"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome,omitempty"`
	Failure string    `json:"failure,omitempty"`
	Work    Work      `json:"work"`
}
type Status struct {
	Version      int    `json:"version"`
	Admitted     uint64 `json:"admitted"`
	Completed    uint64 `json:"completed"`
	LastAdmitted *Pass  `json:"last_admitted,omitempty"`
	// LastCompleted is the newest admitted pass whose completion was recorded;
	// an older concurrent completion cannot overwrite a newer result.
	LastCompleted *Pass `json:"newest_completed,omitempty"`
}
type Evidence struct {
	State      string  `json:"state"`
	Outcome    string  `json:"last_known_outcome"`
	BestEffort bool    `json:"best_effort"`
	Status     *Status `json:"status,omitempty"`
	Note       string  `json:"note"`
}

func decodeStatus(data []byte) (Status, error) {
	var s Status
	if json.Unmarshal(data, &s) != nil || s.Version != 1 || s.Completed > s.Admitted {
		return Status{}, errors.New("maintenance status unavailable")
	}
	for _, p := range []*Pass{s.LastAdmitted, s.LastCompleted} {
		if p == nil {
			continue
		}
		if p.ID == 0 || p.ID > s.Admitted || p.At.IsZero() || p.Work.Classes > 3 {
			return Status{}, errors.New("maintenance status unavailable")
		}
		switch p.Outcome {
		case "", "completed", "deferred", "failed", "interrupted":
		default:
			return Status{}, errors.New("maintenance status unavailable")
		}
		switch p.Failure {
		case "", "io", "timeout", "deferred", "unsafe_object":
		default:
			return Status{}, errors.New("maintenance status unavailable")
		}
	}
	if s.Admitted > 0 && (s.LastAdmitted == nil || s.LastAdmitted.ID != s.Admitted) {
		return Status{}, errors.New("maintenance status unavailable")
	}
	if (s.Admitted == 0) != (s.LastAdmitted == nil) || (s.Completed == 0) != (s.LastCompleted == nil) {
		return Status{}, errors.New("maintenance status unavailable")
	}
	if s.LastAdmitted != nil && (s.LastAdmitted.Outcome != "" || s.LastAdmitted.Failure != "") {
		return Status{}, errors.New("maintenance status unavailable")
	}
	if s.LastCompleted != nil {
		p := s.LastCompleted
		if p.Outcome == "" || ((p.Outcome == "failed" || p.Outcome == "interrupted") != (p.Failure != "")) {
			return Status{}, errors.New("maintenance status unavailable")
		}
	}
	return s, nil
}
func readStatus(ctx context.Context, root string) (Status, error) {
	data, err := statefile.ReadSmallWithin(ctx, root, "coord/maintenance-status.json", statusLimit)
	if errors.Is(err, os.ErrNotExist) {
		return Status{Version: 1}, nil
	}
	if err != nil {
		return Status{}, err
	}
	return decodeStatus(data)
}
func updateStatus(ctx context.Context, root string, update func(*Status) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	return lockfile.Within(ctx, root, "coord/maintenance-status-lock", false, false, func() error {
		s, err := readStatus(ctx, root)
		if err != nil {
			return err
		}
		if err := update(&s); err != nil {
			return err
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if len(data) > statusLimit {
			return errors.New("maintenance status capacity")
		}
		return statefile.WriteSlots(ctx, root, "coord/maintenance-status.json", "coord/.maintenance-status-00", ".maintenance-status-", 1, data)
	})
}
func admitted(ctx context.Context, root string) (uint64, error) {
	var id uint64
	err := updateStatus(ctx, root, func(s *Status) error {
		if s.Admitted == math.MaxUint64 {
			return errors.New("maintenance status capacity")
		}
		s.Admitted++
		id = s.Admitted
		s.LastAdmitted = &Pass{ID: id, At: time.Now().UTC()}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}
func completed(ctx context.Context, root string, id uint64, work Work, outcome, failure string) error {
	if id == 0 {
		return nil
	}
	return updateStatus(ctx, root, func(s *Status) error {
		if id > s.Admitted {
			return errors.New("maintenance status identity")
		}
		if s.Completed < math.MaxUint64 {
			s.Completed++
		}
		if s.LastCompleted == nil || id >= s.LastCompleted.ID {
			s.LastCompleted = &Pass{ID: id, At: time.Now().UTC(), Outcome: outcome, Failure: failure, Work: work}
		}
		return nil
	})
}

// Probe does not run maintenance, create metadata, or infer live work from a
// completion marker. Missing/off/unreadable recording is explicitly unknown.
func Probe(ctx context.Context, root string, recording string) Evidence {
	e := Evidence{State: "unknown", Outcome: "unknown", BestEffort: true, Note: "bounded pass evidence, not proof all housekeeping is finished; markers do not prove a live worker"}
	data, err := statefile.ReadKnown(ctx, root, "coord/maintenance-status.json", statusLimit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		e.State = "never_recorded"
	case err != nil:
		e.State = "unreadable"
	default:
		s, err := decodeStatus(data)
		if err != nil {
			e.State = "unreadable"
			return e
		}
		e.State = "readable"
		e.Status = &s
		switch {
		case s.LastAdmitted == nil:
			e.Outcome = "never_recorded"
		case s.LastCompleted == nil || s.LastCompleted.ID != s.LastAdmitted.ID:
			e.Outcome = "incomplete_evidence"
		default:
			e.Outcome = s.LastCompleted.Outcome
		}
	}
	if recording != "on" {
		e.Outcome = "unknown"
		e.Note += "; current recording " + recording + ", retained status is historical only"
	}
	return e
}
