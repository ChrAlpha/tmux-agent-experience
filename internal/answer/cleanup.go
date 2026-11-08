package answer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/dirscan"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

// Existing producers use six random bytes as a lowercase hexadecimal request
// ID. Other names and file types remain unknown, regardless of owner or age.
var legacySocketName = regexp.MustCompile(`^([0-9a-f]{12})\.sock$`)

func knownRegular(st os.FileInfo) bool {
	return st.Mode().IsRegular() && ownedfs.Owner(st) && st.Sys().(*syscall.Stat_t).Nlink == 1
}

// clearSlot owns its liveness lock and the creator namespace. False means an
// unknown/live object prevents reuse. It never unlinks a live or uncertain
// socket; successful cleanup leaves the slot's fixed names absent.
func clearSlot(ctx context.Context, d *ownedfs.Dir, slot byte, removed func()) (bool, error) {
	remove := func(name string, st os.FileInfo) error {
		err := d.Remove(ctx, name, st)
		if err == nil && removed != nil {
			removed()
		}
		return err
	}
	temp, meta, sock := fmt.Sprintf("temp-%02x", slot), fmt.Sprintf("meta-%02x", slot), fmt.Sprintf("sock-%02x", slot)
	if st, err := d.Stat(ctx, temp); err == nil {
		if !knownRegular(st) {
			return false, nil
		}
		if err := remove(temp, st); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	e, metaErr := readEndpoint(ctx, d, slot)
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		if errors.Is(metaErr, ownedfs.ErrScope) {
			return false, nil
		}
		return false, metaErr
	}
	st, err := d.Stat(ctx, sock)
	if err == nil {
		if metaErr != nil || st.Mode()&os.ModeSocket == 0 || !ownedfs.Owner(st) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
			return false, nil //nolint:nilerr // Missing identity preserves an existing socket and forbids reuse.
		}
		if probeAt(ctx, d, sock, e.Request) != Dead {
			return false, nil
		}
		if err := remove(sock, st); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if metaErr == nil {
		st, err := d.Stat(ctx, meta)
		if err != nil {
			return false, err
		}
		if !knownRegular(st) {
			return false, nil
		}
		if err := remove(meta, st); err != nil {
			return false, err
		}
	}
	return true, nil
}

type CleanupReport struct {
	Examined   int
	Candidates int
	Removed    int
	Slots      int
	Deferred   int
}

// Cleanup runs only after explicit retirement. Fixed positions need no growing
// enumeration. Known flat legacy sockets use a native-page cursor; each pass
// resumes beyond unknown prefixes. All creators enter the gate before binding,
// and fixed listeners retain their slot lock until their socket has closed.
func Cleanup(ctx context.Context, root string) (CleanupReport, error) {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	var r CleanupReport
	err := lockfile.Within(ctx, root, "coord/gate", false, false, func() error {
		mode, err := coordModeUnderGate(ctx, root)
		if err != nil || mode != coord.Pool {
			return err
		}
		d, err := ownedfs.Open(ctx, root, "req", false)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ownedfs.ErrScope) {
			return nil
		}
		if err != nil {
			return err
		}
		defer d.Close()
		start := uint64(0)
		b, err := statefile.ReadSmallWithin(ctx, root, "coord/request-sweep", 8)
		if err == nil {
			start, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 8)
			if err != nil {
				return ownedfs.ErrScope
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := statefile.WriteWithin(ctx, root, "coord/request-sweep", "coord/request-sweep.tmp", []byte(fmt.Sprintf("%d\n", (start+1)%socketSlots))); err != nil {
			return err
		}
		// The low bit alternates which class gets first use of a short remaining
		// parent budget. The fixed position itself advances before any removals.
		fixed := func() error {
			for _, prefix := range []string{"meta-", "temp-", "sock-"} {
				claim, err := ownedfs.OpenSlots(ctx, root, "req", prefix, socketSlots, false)
				if errors.Is(err, ownedfs.ErrScope) {
					return nil
				}
				if err != nil {
					return err
				}
				claim.Close()
			}
			for i := range 4 {
				if err := ctx.Err(); err != nil {
					return err
				}
				slot := byte(int(start) + i)
				r.Slots++
				err := lockfile.Within(ctx, root, fmt.Sprintf("coord/r-%02x", slot), false, false, func() error {
					r.Examined += 3
					_, err := clearSlot(ctx, d, slot, func() { r.Removed++ })
					if err != nil {
						return err
					}
					return nil
				})
				if errors.Is(err, lockfile.ErrBusy) {
					r.Deferred++
				} else if err != nil {
					return err
				}
			}
			return nil
		}
		legacy := func() error { return cleanupFlat(ctx, d, root, &r) }
		if start%2 == 0 {
			if err := fixed(); err != nil {
				return err
			}
			return legacy()
		}
		if err := legacy(); err != nil {
			return err
		}
		return fixed()
	})
	if errors.Is(err, lockfile.ErrBusy) {
		r.Deferred++
		return r, nil
	}
	if parentCtx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		r.Deferred++
		err = contextio.WithoutContextError(err, ctx.Err())
	}
	return r, err
}

// Current would try to reenter the gate exclusively held by this maintenance
// pass. Read its marker through the same pinned scope instead.
func coordModeUnderGate(ctx context.Context, root string) (coord.Mode, error) {
	b, err := statefile.ReadSmallWithin(ctx, root, "coord/mode", 32)
	if errors.Is(err, os.ErrNotExist) {
		return coord.Compatibility, nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) != string(coord.Pool) {
		return "", ownedfs.ErrScope
	}
	return coord.Pool, nil
}

func cleanupFlat(ctx context.Context, d *ownedfs.Dir, root string, r *CleanupReport) error {
	var cursor dirscan.Cursor
	b, err := statefile.ReadSmallWithin(ctx, root, "coord/old-request-sweep", 16<<10)
	if err == nil {
		if json.Unmarshal(b, &cursor) != nil || !cursor.Valid() {
			return ownedfs.ErrScope
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	names, next, examined, err := dirscan.Next(ctx, d, cursor, 8)
	r.Examined += examined
	if err != nil {
		return err
	}
	data, _ := json.Marshal(next)
	if err := statefile.WriteWithin(ctx, root, "coord/old-request-sweep", "coord/old-request-sweep.tmp", data); err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.Candidates++
		parts := legacySocketName.FindStringSubmatch(name)
		if parts == nil {
			continue
		}
		st, err := d.Stat(ctx, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSocket == 0 || !ownedfs.Owner(st) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
			continue
		}
		if probeAt(ctx, d, name, parts[1]) != Dead {
			continue
		}
		if err := d.Remove(ctx, name, st); err != nil {
			if errors.Is(err, ownedfs.ErrScope) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		r.Removed++
	}
	return nil
}
