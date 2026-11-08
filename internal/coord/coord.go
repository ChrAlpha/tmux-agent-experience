// Package coord provides stable resource lock pools and an explicit transition
// from legacy per-resource locks. It does not infer retirement from filesystem
// contents, process absence, or successfully acquiring a lock.
package coord

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
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
	return filepath.Join(root(dir), fmt.Sprintf("%s-%02x", class, Slot(key)))
}

// Slot is the frozen pool-v1 mapping: 256 slots, selected by the first byte of
// the resource's existing SHA-256 key. Future versions must retain this mapping
// and the class namespaces while they can share a state directory.
func Slot(key [32]byte) byte { return key[0] }

// TrySlot excludes creators of new fixed artifacts without waiting. Those
// creators always hold the pool, including in compatibility mode. This does
// not acquire a legacy lock and must never be used to change session/pane state.
func TrySlot(ctx context.Context, dir string, class Class, slot byte, fn func() error) error {
	if class != Pane && class != Session {
		return fmt.Errorf("invalid coordination class %q", class)
	}
	return lockfile.Within(ctx, dir, "coord/gate", true, false, func() error {
		if _, err := readMode(ctx, dir); err != nil {
			return err
		}
		return lockfile.Within(ctx, dir, filepath.Join("coord", fmt.Sprintf("%s-%02x", class, slot)), false, false, fn)
	})
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
	return lockfile.Within(ctx, dir, "coord/gate", true, true, func() error {
		mode, err := readMode(ctx, dir)
		if err != nil {
			return err
		}
		return lockfile.Within(ctx, dir, filepath.Join("coord", fmt.Sprintf("%s-%02x", class, Slot(key))), false, true, func() error {
			if mode == Compatibility {
				return lockfile.Within(ctx, dir, filepath.Join("locks", string(class)+"-"+hex.EncodeToString(key[:8])), false, true, fn)
			}
			return fn()
		})
	})
}

// Current reads the mode while participating in its namespace.
func Current(ctx context.Context, dir string) (Mode, error) {
	var mode Mode
	err := lockfile.Within(ctx, dir, "coord/gate", true, true, func() error {
		var err error
		mode, err = readMode(ctx, dir)
		return err
	})
	return mode, err
}

func readMode(ctx context.Context, dir string) (Mode, error) {
	b, err := statefile.ReadSmallWithin(ctx, dir, "coord/mode", 32)
	if errors.Is(err, os.ErrNotExist) {
		return Compatibility, nil
	}
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
	return lockfile.Within(ctx, dir, "coord/gate", false, true, func() error {
		mode, err := readMode(ctx, dir)
		if err != nil || mode == Pool {
			return err
		}
		// Freeze legacy directory identity before publishing retirement evidence.
		// Pin empty anchors too, so a later inserted real directory cannot become
		// owned merely because it appears under a familiar legacy basename.
		for _, name := range []string{"locks", "req"} {
			scope, err := ownedfs.Open(ctx, dir, name, true)
			if err != nil {
				return err
			}
			scope.Close()
		}
		return statefile.WriteWithin(ctx, dir, "coord/mode", "coord/mode.tmp", []byte(string(Pool)+"\n"))
	})
}
