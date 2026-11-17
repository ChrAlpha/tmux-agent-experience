// Package claudestatus reads the status file Claude Code writes for each of
// its sessions. A turn that no hook reports, such as one a person interrupted,
// shows there as idle; tmag reads only the fields that prove it and opens
// nothing else in the directory.
package claudestatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
)

// MaxBytes bounds a status file. Claude Code writes a small JSON object; a
// larger file is not one tmag reads.
const MaxBytes = 64 << 10

// Status is what tmag reads of Claude Code's session status: the session it
// belongs to, the process that wrote it, and when it last changed.
type Status struct {
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // epoch milliseconds
	ProcStart       string `json:"procStart"`
}

// Read opens only path and decodes the status fields tmag reads. The open is
// non-blocking and the opened file must be regular before it is read: a FIFO,
// device or directory named as a status file changes nothing and cannot hold
// a reader past its context. Symlinks are followed, as the file Claude Code
// writes may be reached through one.
func Read(ctx context.Context, path string) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Status{}, err
	}
	if !st.Mode().IsRegular() {
		return Status{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := contextio.ReadAtMost(f, MaxBytes)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("decode Claude status file: %w", err)
	}
	return s, nil
}

// IdleAfter reports whether s proves the session idle after its last hook
// contact: it belongs to that session and process, and reports idle at a
// later time. A foreign, missing or stale status changes nothing.
func (s Status) IdleAfter(sessionID, procStart string, lastHookAt time.Time) bool {
	if s.SessionID != sessionID || s.ProcStart != procStart || s.Status != "idle" || s.StatusUpdatedAt <= 0 {
		return false
	}
	return time.UnixMilli(s.StatusUpdatedAt).After(lastHookAt)
}
