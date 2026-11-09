// Package coord provides fixed resource lock pools for one supported writer
// protocol per dedicated state directory. Legacy migration is an explicit
// operator declaration; absence of old processes or locks never proves it.
package coord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

type Class string

const (
	Pane    Class = "p"
	Session Class = "s"
	Slots         = 256
)

type Mode string

// Pool identifies the dedicated-directory protocol. Older pool-v1 binaries
// refuse this marker: sharing a directory with them is not supported.
const Pool Mode = "pool-v2"

var ErrMigration = errors.New("legacy or uninitialized state: stop tmag invocations, upgrade their launch paths, then run tmag prune --retire-legacy-locks from your own terminal; keep older binaries out of this directory")

func root(dir string) string { return filepath.Join(dir, "coord") }

// Gate and every pool inode are permanent, including after migration.
func Gate(dir string) string { return filepath.Join(root(dir), "gate") }

// Slot is the fixed pool mapping, retaining the existing resource SHA-256.
func Slot(key [32]byte) byte { return key[0] }

// With holds the namespace and one stripe. Acquisition order is pane, session,
// eventlog. Never acquire a second colliding stripe of the same class recursively.
func With(ctx context.Context, dir string, class Class, key [32]byte, fn func() error) error {
	if class != Pane && class != Session {
		return fmt.Errorf("invalid coordination class %q", class)
	}
	if _, err := Current(ctx, dir); err != nil {
		return err
	}
	return lockfile.Within(ctx, dir, "coord/gate", true, true, func() error {
		if _, err := readMode(ctx, dir); err != nil {
			return err
		}
		return lockfile.Within(ctx, dir, filepath.Join("coord", fmt.Sprintf("%s-%02x", class, Slot(key))), false, true, fn)
	})
}

// Current initializes an empty directory directly into the fixed layout. Any
// existing state without this marker requires explicit quiescent migration.
// The read-only fast path also permits nested pane/session locking.
func Current(ctx context.Context, dir string) (Mode, error) {
	mode, err := readMode(ctx, dir)
	if !errors.Is(err, os.ErrNotExist) {
		return mode, err
	}
	err = lockfile.Within(ctx, dir, "coord/gate", false, true, func() error {
		if _, err := readMode(ctx, dir); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Only the just-created coordination directory and its gate may exist.
		// Read at most two names, not an unbounded legacy-state scan.
		for _, rel := range []string{".", "coord"} {
			d, err := ownedfs.Open(ctx, dir, rel, false)
			if err != nil {
				return err
			}
			f, err := d.File(ctx)
			if err != nil {
				d.Close()
				return err
			}
			names, err := f.Readdirnames(2)
			f.Close()
			valid := len(names) == 1 && ((rel == "." && names[0] == "coord") || (rel == "coord" && names[0] == "gate"))
			if err != nil && !errors.Is(err, io.EOF) {
				d.Close()
				return err
			}
			if err := d.Validate(ctx); err != nil {
				d.Close()
				return err
			}
			d.Close()
			if !valid {
				return ErrMigration
			}
		}
		return writeMode(ctx, dir)
	})
	if err != nil {
		return "", err
	}
	return Pool, nil
}

func readMode(ctx context.Context, dir string) (Mode, error) {
	b, err := statefile.ReadSmallWithin(ctx, dir, "coord/mode", 32)
	if err != nil {
		return "", err
	}
	if string(b) != string(Pool)+"\n" {
		return "", fmt.Errorf("%w (unsupported coord/mode; unknown formats need a fresh dedicated state directory)", ErrMigration)
	}
	return Pool, nil
}

func writeMode(ctx context.Context, dir string) error {
	return statefile.WriteWithin(ctx, dir, "coord/mode", "coord/mode.tmp", []byte(string(Pool)+"\n"))
}

// RetireLegacy declares that all tmag invocations and waiters have finished and
// all launch paths now select this protocol. The CLI restricts the declaration
// to the local person. It neither tests for retirement nor removes legacy files.
// Keep older writers and rollback binaries out of this directory afterward.
func RetireLegacy(ctx context.Context, dir string) error {
	return lockfile.Within(ctx, dir, "coord/gate", false, true, func() error {
		b, err := statefile.ReadSmallWithin(ctx, dir, "coord/mode", 32)
		if err == nil {
			if string(b) == string(Pool)+"\n" {
				return nil
			}
			if string(b) != "pool-v1\n" {
				return errors.New("unknown state format; preserve this directory and select a fresh dedicated TMAG_STATE_DIR")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeMode(ctx, dir)
	})
}
