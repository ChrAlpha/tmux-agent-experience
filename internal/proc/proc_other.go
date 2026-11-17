//go:build !linux

package proc

import (
	"context"
	"errors"
)

// readProcFS is unavailable without /proc; Snapshot falls back
// to ps.
func readProcFS(context.Context) ([]Process, error) {
	return nil, errors.New("proc: no /proc")
}

// readProcEntry is unavailable without /proc; Running uses the
// process table there.
func readProcEntry(context.Context, int) (Process, bool) {
	return Process{}, false
}

// readProcLineage is unavailable without /proc; Lineage uses
// the full table from ps.
func readProcLineage(context.Context, int) (*Table, error) {
	return nil, errors.New("proc: no /proc")
}
