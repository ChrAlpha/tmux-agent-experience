//go:build !linux

package proc

import "context"

// snapshot reads every process from ps.
func snapshot(ctx context.Context) ([]Process, error) { return readPS(ctx) }

// lineage reads the full table from ps: without /proc no single process can
// be read alone.
func lineage(ctx context.Context, _ int) (*Table, error) {
	t, err := Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	t.complete = false
	return t, nil
}

// running reads the full table from ps.
func running(ctx context.Context, pid int, start string) bool {
	t, err := Snapshot(ctx)
	return err != nil || t.Alive(pid, start)
}
