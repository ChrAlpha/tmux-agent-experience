package coord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/dirscan"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

var retiredName = regexp.MustCompile(`^[ps]-[0-9a-f]{16}$`)

// Reclaimed reports actual examination/removal counts. Unknown directory
// entries are charged to the page budget too, rather than filtered for free.
type Reclaimed struct {
	Examined   int
	Candidates int
	Removed    int
}

// CleanupRetired removes known empty legacy lock files only after the explicit
// operator retirement declaration. The exclusive stable gate excludes all new
// creators and pre-opened cooperating waiters; older binaries ignore it and
// must already be retired under that declaration's documented preconditions.
func CleanupRetired(ctx context.Context, root string) (Reclaimed, error) {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	var r Reclaimed
	err := lockfile.Within(ctx, root, "coord/gate", false, false, func() error {
		mode, err := readMode(ctx, root)
		if err != nil || mode != Pool {
			return err
		}
		d, err := ownedfs.Open(ctx, root, "locks", false)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ownedfs.ErrScope) {
			return nil
		}
		if err != nil {
			return err
		}
		defer d.Close()
		var cursor dirscan.Cursor
		b, err := statefile.ReadSmallWithin(ctx, root, "coord/retired-sweep", 16<<10)
		if err == nil {
			if json.Unmarshal(b, &cursor) != nil || !cursor.Valid() {
				return ownedfs.ErrScope
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		names, next, examined, err := dirscan.Next(ctx, d, cursor, 32)
		r.Examined = examined
		if err != nil {
			return err
		}
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := statefile.WriteWithin(ctx, root, "coord/retired-sweep", "coord/retired-sweep.tmp", data); err != nil {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.Candidates++
			if !retiredName.MatchString(name) {
				continue
			}
			st, err := d.Stat(ctx, name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() || st.Size() != 0 || !ownedfs.Owner(st) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
				continue
			}
			f, err := d.OpenFile(ctx, name, os.O_RDWR, 0)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				if errors.Is(err, ownedfs.ErrScope) {
					continue
				}
				return err
			}
			removed, err := func() (bool, error) {
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
						return false, nil
					}
					return false, err
				}
				held, err := f.Stat()
				if err != nil {
					return false, err
				}
				if !os.SameFile(st, held) {
					return false, nil
				}
				err = d.Remove(ctx, name, held)
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, ownedfs.ErrScope) {
					return false, nil
				}
				return err == nil, err
			}()
			if err != nil {
				return err
			}
			if removed {
				r.Removed++
			}
		}
		return nil
	})
	if errors.Is(err, lockfile.ErrBusy) {
		return r, nil
	}
	if parentCtx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		err = nil
	}
	return r, err
}
