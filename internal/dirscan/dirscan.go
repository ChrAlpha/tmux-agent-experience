// Package dirscan advances a directory using one bounded native page, not a
// growing read-all or a replay of its prefix. Saved cookies are filesystem
// positions, meaningful only for the same pinned directory inode.
package dirscan

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
)

const PageBytes = 4096
const MaxNames = 256

// Cursor bounds both pending work and metadata size. A stale/invalid filesystem
// cookie resets the sweep; it never authorizes deletion or proves ownership.
type Cursor struct {
	Device  int64    `json:"device"`
	Inode   uint64   `json:"inode"`
	Offset  int64    `json:"offset"`
	Pending []string `json:"pending,omitempty"`
}

func (c Cursor) Valid() bool {
	if c.Offset < 0 || len(c.Pending) > MaxNames {
		return false
	}
	for _, name := range c.Pending {
		if name == "" || name == "." || name == ".." || len(name) > 1024 || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
			return false
		}
	}
	return true
}

// Next examines at most one 4KiB native directory page, or a saved page's
// remaining names. It returns at most limit names. Save next BEFORE acting on
// names: death or a deadline may defer an entry until the next circular sweep,
// but neither restarts at an unknown prefix nor loses the cursor indefinitely.
func Next(ctx context.Context, dir *ownedfs.Dir, c Cursor, limit int) (names []string, next Cursor, examined int, err error) {
	if limit < 1 || limit > MaxNames || !c.Valid() {
		return nil, c, 0, ownedfs.ErrScope
	}
	f, err := dir.File(ctx)
	if err != nil {
		return nil, c, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, c, 0, err
	}
	sys := st.Sys().(*syscall.Stat_t)
	if c.Device != int64(sys.Dev) || c.Inode != sys.Ino {
		c = Cursor{Device: int64(sys.Dev), Inode: sys.Ino}
	}
	if len(c.Pending) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, c, 0, err
		}
		if _, err := syscall.Seek(int(f.Fd()), c.Offset, io.SeekStart); err != nil {
			if !errors.Is(err, syscall.EINVAL) {
				return nil, c, 0, err
			}
			c.Offset = 0
			if _, err := syscall.Seek(int(f.Fd()), 0, io.SeekStart); err != nil {
				return nil, c, 0, err
			}
		}
		buf := make([]byte, PageBytes)
		n, err := readPage(int(f.Fd()), buf)
		if err != nil {
			if errors.Is(err, syscall.EINVAL) {
				c.Offset = 0
				return nil, c, 0, nil
			}
			return nil, c, 0, err
		}
		if err := dir.Validate(ctx); err != nil {
			return nil, c, 0, err
		}
		if n == 0 {
			c.Offset = 0
			return nil, c, 0, nil
		}
		_, count, pending := syscall.ParseDirent(buf[:n], MaxNames, nil)
		if count == MaxNames {
			return nil, c, 0, errors.New("native directory page exceeded entry bound")
		}
		c.Pending = pending
		c.Offset, err = syscall.Seek(int(f.Fd()), 0, io.SeekCurrent)
		if err != nil {
			return nil, c, 0, err
		}
		examined = count
	}
	n := min(limit, len(c.Pending))
	names = append([]string(nil), c.Pending[:n]...)
	c.Pending = c.Pending[n:]
	return names, c, examined, ctx.Err()
}
