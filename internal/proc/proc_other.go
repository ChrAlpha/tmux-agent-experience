//go:build !linux

package proc

import (
	"context"
	"errors"
)

// readProcFSContext is unavailable without /proc; SnapshotContext falls back
// to ps.
func readProcFSContext(context.Context) ([]Process, error) {
	return nil, errors.New("proc: no /proc")
}

// readProcEntryContext is unavailable without /proc; RunningContext uses the
// process table there.
func readProcEntryContext(context.Context, int) (Process, bool) {
	return Process{}, false
}

// readProcLineageContext is unavailable without /proc; LineageContext uses
// the full table from ps.
func readProcLineageContext(context.Context, int) (*Table, error) {
	return nil, errors.New("proc: no /proc")
}
