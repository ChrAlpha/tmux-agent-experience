package statefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

// ErrReadCapacity means the diagnostic cannot inspect the whole file within its bound.
var ErrReadCapacity = errors.New("diagnostic metadata exceeds limit")

// ReadKnown is a bounded diagnostic read without creating or adopting metadata.
func ReadKnown(ctx context.Context, root, path string, limit int64) ([]byte, error) {
	d, err := ownedfs.OpenKnown(ctx, root, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.OpenFile(ctx, filepath.Base(path), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !owned(st) {
		return nil, errors.New("unreadable diagnostic metadata")
	}
	if st.Size() > limit {
		return nil, ErrReadCapacity
	}
	b, err := contextio.ReadAll(ctx, f, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrReadCapacity
	}
	return b, ctx.Err()
}
