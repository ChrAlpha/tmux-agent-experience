// Package statefile publishes bounded, owned metadata through fixed, excluded
// temporaries. Both names are direct children of one pinned directory, so an
// ancestor replacement cannot redirect atomic publication or reclamation.
package statefile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

func owned(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return st.Mode().IsRegular() && ok && s.Uid == uint32(os.Geteuid()) && s.Nlink == 1
}

// Write is a fixture compatibility helper. Production uses an explicit selected
// root through WriteWithin; the caller excludes the temporary.
func Write(ctx context.Context, path, tmp string, data []byte) error {
	if filepath.Dir(path) != filepath.Dir(tmp) {
		return ownedfs.ErrScope
	}
	return WriteWithin(ctx, filepath.Dir(path), filepath.Base(path), filepath.Base(tmp), data)
}

// WriteWithin publishes metadata using a known temporary in the same directory.
func WriteWithin(ctx context.Context, root, path, tmp string, data []byte) error {
	if filepath.Dir(path) != filepath.Dir(tmp) {
		return ownedfs.ErrScope
	}
	d, err := ownedfs.Open(ctx, root, filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer d.Close()
	return write(ctx, d, filepath.Base(path), filepath.Base(tmp), data)
}

func write(ctx context.Context, d *ownedfs.Dir, path, tmp string, data []byte) error {
	f, err := d.OpenFile(ctx, tmp, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !owned(st) {
		return ownedfs.ErrScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(len(data), 64<<10)
		if _, err := f.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	if err := f.Close(); err != nil {
		return err
	}
	return d.Rename(ctx, tmp, d, path, st)
}

func ReadSmall(ctx context.Context, path string, limit int64) ([]byte, error) {
	return ReadSmallWithin(ctx, filepath.Dir(path), filepath.Base(path), limit)
}

// ReadSmallWithin refuses uncertain ancestry, aliases and oversized metadata.
func ReadSmallWithin(ctx context.Context, root, path string, limit int64) ([]byte, error) {
	d, err := ownedfs.Open(ctx, root, filepath.Dir(path), false)
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
	if !owned(st) || st.Size() > limit {
		return nil, fmt.Errorf("unowned or oversized metadata %s", filepath.Base(path))
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("metadata grew beyond its limit")
	}
	return b, ctx.Err()
}

func Remove(ctx context.Context, path string) (bool, error) {
	return RemoveWithin(ctx, filepath.Dir(path), filepath.Base(path))
}

func RemoveWithin(ctx context.Context, root, path string) (bool, error) {
	d, err := ownedfs.Open(ctx, root, filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer d.Close()
	return remove(ctx, d, filepath.Base(path))
}

func remove(ctx context.Context, d *ownedfs.Dir, name string) (bool, error) {
	st, err := d.Stat(ctx, name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !owned(st) {
		return false, nil
	}
	if err := d.Remove(ctx, name, st); err != nil {
		return false, err
	}
	return true, nil
}
