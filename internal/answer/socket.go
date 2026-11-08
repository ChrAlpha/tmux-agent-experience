package answer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/socketbind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const socketSlots = 256
const setupBudget = time.Second

var requestName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type endpoint struct {
	Request string `json:"request"`
}

type listener struct {
	*net.UnixListener
	id   string
	dir  *ownedfs.Dir
	slot *lockfile.Held
	once sync.Once
	err  error
}

// Close owns no filesystem cleanup. It cannot unlink a rebound endpoint or
// spend a fresh budget after cancellation; a fixed slot or retired-legacy sweep
// reclaims its orphan later. Closing the liveness descriptor releases exclusion.
func (l *listener) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if l.slot != nil {
			l.err = errors.Join(l.err, l.slot.Close())
		}
		l.err = errors.Join(l.err, l.dir.Close())
	})
	return l.err
}

// Listen retains the bounded setup compatibility API. Operation-bearing callers
// use ListenContext so their earlier deadline also contains setup.
func Listen(root, id string) (net.Listener, error) {
	return ListenContext(context.Background(), root, id)
}

// ListenContext bounds setup, separately from Wait's intentional lifetime.
// Compatibility retains the old flat pathname, refusing every existing object.
// After retirement a pool allocates at most 256 simultaneous listeners and
// bounded fixed metadata/socket positions. Exhaustion fails open to the dialog.
func ListenContext(ctx context.Context, root, id string) (net.Listener, error) {
	if !requestName.MatchString(id) {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, setupBudget)
	defer cancel()
	var result net.Listener
	gate, err := lockfile.AcquireWithin(ctx, root, "coord/gate", true, true)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	allocation, err := lockfile.AcquireWithin(ctx, root, "coord/request-allocation", false, true)
	if err != nil {
		return nil, err
	}
	defer allocation.Close()
	err = func() error {
		// Current uses a shared gate too; lock order remains namespace before
		// request-allocation before a request slot, never a pane/session/log lock.
		mode, err := coord.Current(ctx, root)
		if err != nil {
			return err
		}
		if mode == coord.Compatibility {
			d, err := ownedfs.Open(ctx, root, "req", true)
			if err != nil {
				return err
			}
			ln, err := bind(ctx, d, id+".sock", id, nil, gate.File(), allocation.File())
			if err != nil {
				d.Close()
				return err
			}
			result = ln
			return nil
		}
		return func() error {
			var d *ownedfs.Dir
			defer func() {
				if d != nil {
					d.Close()
				}
			}()
			for _, prefix := range []string{"meta-", "temp-", "sock-"} {
				claim, err := ownedfs.OpenSlots(ctx, root, "req", prefix, socketSlots, true)
				if err != nil {
					return err
				}
				if d != nil {
					d.Close()
				}
				d = claim
			}

			// Duplicate IDs never acquire another slot. Published metadata is only
			// request identity; no tool name/input or decision reaches disk.
			if previous, found, err := locate(ctx, d, id); err != nil {
				return err
			} else if found {
				held, err := lockfile.AcquireWithin(ctx, root, fmt.Sprintf("coord/r-%02x", previous), false, false)
				if err != nil {
					return err
				}
				free, err := clearSlot(ctx, d, previous, nil)
				held.Close()
				if err != nil {
					return err
				}
				if !free {
					return errors.New("request endpoint is live or uncertain")
				}
			}
			start := sha256.Sum256([]byte(id))
			for i := range socketSlots {
				if err := ctx.Err(); err != nil {
					return err
				}
				slot := byte(int(start[0]) + i)
				held, err := lockfile.AcquireWithin(ctx, root, fmt.Sprintf("coord/r-%02x", slot), false, false)
				if errors.Is(err, lockfile.ErrBusy) {
					continue
				}
				if err != nil {
					return err
				}
				free, err := clearSlot(ctx, d, slot, nil)
				if err != nil {
					held.Close()
					return err
				}
				if !free {
					held.Close()
					continue
				}
				data, _ := json.Marshal(endpoint{Request: id})
				meta, tmp := fmt.Sprintf("req/meta-%02x", slot), fmt.Sprintf("req/temp-%02x", slot)
				if err := statefile.WriteWithin(ctx, root, meta, tmp, data); err != nil {
					held.Close()
					return err
				}
				ln, err := bind(ctx, d, fmt.Sprintf("sock-%02x", slot), id, held, gate.File(), allocation.File(), held.File())
				if err != nil {
					held.Close()
					return err
				}
				d = nil
				result = ln
				return nil
			}
			return errors.New("all answer socket positions are occupied or uncertain")
		}()
	}()
	return result, err
}

// bind creates through a short-lived child whose cwd is the pinned directory.
// Exclusion descriptors survive parent death until that child exits. The
// returned inode identity must match the entry in the still-valid realm scope.
func bind(ctx context.Context, d *ownedfs.Dir, name, id string, held *lockfile.Held, keep ...*os.File) (net.Listener, error) {
	if _, err := d.Stat(ctx, name); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, errors.New("answer endpoint already exists")
		}
		return nil, err
	}
	anchor, err := d.File(ctx)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	bound, err := socketbind.Listen(ctx, anchor, name, keep...)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			bound.Listener.Close()
		}
	}()
	st, err := d.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	sys := st.Sys().(*syscall.Stat_t)
	if st.Mode()&os.ModeSocket == 0 || !ownedfs.Owner(st) || sys.Nlink != 1 || int64(sys.Dev) != bound.Device || sys.Ino != bound.Inode {
		return nil, ownedfs.ErrScope
	}
	if err := d.Validate(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	success = true
	return &listener{UnixListener: bound.Listener, id: id, dir: d, slot: held}, nil
}

func readEndpoint(ctx context.Context, d *ownedfs.Dir, slot byte) (endpoint, error) {
	f, err := d.OpenFile(ctx, fmt.Sprintf("meta-%02x", slot), os.O_RDONLY, 0)
	if err != nil {
		return endpoint{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 513))
	if err != nil {
		return endpoint{}, err
	}
	var e endpoint
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if len(b) > 512 || decoder.Decode(&e) != nil || !requestName.MatchString(e.Request) {
		return e, ownedfs.ErrScope
	}
	var tail any
	if err := decoder.Decode(&tail); !errors.Is(err, io.EOF) {
		return e, ownedfs.ErrScope
	}
	return e, ctx.Err()
}

func locate(ctx context.Context, d *ownedfs.Dir, id string) (byte, bool, error) {
	start := sha256.Sum256([]byte(id))
	var found bool
	var selected byte
	for i := range socketSlots {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		slot := byte(int(start[0]) + i)
		e, err := readEndpoint(ctx, d, slot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		// An unreadable record may already name id. Neither a missing flat path
		// nor another free slot can turn uncertain identity into proven absence.
		if err != nil {
			return 0, false, err
		}
		if e.Request == id {
			if found {
				return 0, false, ownedfs.ErrScope
			}
			found = true
			selected = slot
		}
	}
	return selected, found, nil
}

// lookup retains the pinned scope through connect and identity validation.
func lookup(ctx context.Context, root, id string) (*ownedfs.Dir, string, bool, error) {
	if !requestName.MatchString(id) {
		return nil, "", false, ErrInvalid
	}
	d, err := ownedfs.Open(ctx, root, "req", false)
	if err != nil {
		return nil, "", false, err
	}
	success := false
	defer func() {
		if !success {
			d.Close()
		}
	}()
	mode, err := coord.Current(ctx, root)
	if err != nil {
		return nil, "", false, err
	}
	name := id + ".sock"
	pooled := false
	if mode == coord.Pool {
		slot, found, err := locate(ctx, d, id)
		if err != nil {
			return nil, "", false, err
		}
		if found {
			name = fmt.Sprintf("sock-%02x", slot)
			pooled = true
		}
	}
	if _, err := d.Stat(ctx, name); err != nil {
		return nil, "", false, err
	}
	if err := d.Validate(ctx); err != nil {
		return nil, "", false, err
	}
	success = true
	return d, name, pooled, nil
}

// location is a contextual path helper for fixtures/diagnostics. Production
// retains lookup's pinned handle for socketbind.Dial, on Linux and Darwin.
func location(ctx context.Context, root, id string) (string, error) {
	d, name, _, err := lookup(ctx, root, id)
	if err != nil {
		return "", err
	}
	defer d.Close()
	return filepath.Join(root, "req", name), nil
}

func dialAt(ctx context.Context, d *ownedfs.Dir, name string) (net.Conn, error) {
	before, err := d.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSocket == 0 || !ownedfs.Owner(before) {
		return nil, ownedfs.ErrScope
	}
	anchor, err := d.File(ctx)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	connected, err := socketbind.Dial(ctx, anchor, name)
	if err != nil {
		// A refusal is meaningful only while the original directory and entry
		// still establish this endpoint's identity.
		after, scopeErr := d.Stat(ctx, name)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if !os.SameFile(before, after) {
			return nil, ownedfs.ErrScope
		}
		return nil, err
	}
	after, err := d.Stat(ctx, name)
	if err != nil {
		connected.Conn.Close()
		return nil, err
	}
	sys := after.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, after) || !ownedfs.Owner(after) || int64(sys.Dev) != connected.Device || sys.Ino != connected.Inode {
		connected.Conn.Close()
		return nil, ownedfs.ErrScope
	}
	if err := d.Validate(ctx); err != nil {
		connected.Conn.Close()
		return nil, err
	}
	return connected.Conn, nil
}
