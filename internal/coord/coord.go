// Package coord provides stable resource lock pools and an explicit transition
// from legacy per-resource locks. It does not infer retirement from filesystem
// contents, process absence, or successfully acquiring a lock.
package coord

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
)

// Class separates lock pools even when resource hashes collide. The acquisition
// order remains pane -> session -> eventlog. A callback holds only one resource
// of a class: acquiring a second colliding stripe recursively would deadlock.
type Class string

const (
	Pane    Class = "p"
	Session Class = "s"
	Slots         = 256
)

// Mode says which coordination protocol this state directory admits.
type Mode string

const (
	Compatibility Mode = "compatibility"
	Pool          Mode = "pool-v1"
)

func root(dir string) string { return filepath.Join(dir, "coord") }

// Gate is permanent. It and the pool files are never unlinked by maintenance.
func Gate(dir string) string { return filepath.Join(root(dir), "gate") }

func poolPath(dir string, class Class, key [32]byte) string {
	return filepath.Join(root(dir), fmt.Sprintf("%s-%02x", class, key[0]))
}

func legacyPath(dir string, class Class, key [32]byte) string {
	return filepath.Join(dir, "locks", string(class)+"-"+hex.EncodeToString(key[:8]))
}

// With holds a stripe and, while compatibility is required, the exact legacy
// inode old writers use. The shared gate begins before any resource lock opens
// and remains held until every descriptor closes, including while waiting.
// Older writers do not participate in the gate and are excluded only by their
// legacy lock; the gate itself never proves that those writers have retired.
func With(ctx context.Context, dir string, class Class, key [32]byte, fn func() error) error {
	if class != Pane && class != Session {
		return fmt.Errorf("invalid coordination class %q", class)
	}
	return lockfile.Shared(ctx, Gate(dir), func() error {
		mode, err := readMode(dir)
		if err != nil {
			return err
		}
		return lockfile.With(ctx, poolPath(dir, class, key), func() error {
			if mode == Compatibility {
				return lockfile.With(ctx, legacyPath(dir, class, key), fn)
			}
			return fn()
		})
	})
}

// Current reads the mode while participating in its namespace.
func Current(ctx context.Context, dir string) (Mode, error) {
	var mode Mode
	err := lockfile.Shared(ctx, Gate(dir), func() error {
		var err error
		mode, err = readMode(dir)
		return err
	})
	return mode, err
}

func readMode(dir string) (Mode, error) {
	f, err := os.OpenFile(filepath.Join(root(dir), "mode"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		// Empty/new directories require the same explicit declaration as old
		// ones: absence of legacy files does not exclude a future old writer.
		return Compatibility, nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > 32 {
		return "", errors.New("invalid coordination mode file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) != string(Pool) {
		return "", errors.New("unknown coordination mode; refusing a different lock protocol")
	}
	return Pool, nil
}

// RetireLegacy is an explicit operator declaration, not an automatic detector.
// Its caller must establish that old tmag processes (including answer hooks,
// serve and lock waiters) finished, all hook launch paths use the new binary,
// and no old writer will restart against dir. Those preconditions continue to
// hold after success. The CLI restricts this act to the local person's command.
// No further confirmation follows that command. The persistent marker is the
// retirement evidence for future maintenance, which still takes the gate.
func RetireLegacy(ctx context.Context, dir string) error {
	return lockfile.With(ctx, Gate(dir), func() error {
		mode, err := readMode(dir)
		if err != nil || mode == Pool {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		tmp := filepath.Join(root(dir), "mode.tmp")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || !ok || sys.Nlink != 1 || sys.Uid != uint32(os.Geteuid()) {
			return errors.New("invalid coordination temporary file")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.WriteString(string(Pool) + "\n"); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(root(dir), "mode"))
	})
}
