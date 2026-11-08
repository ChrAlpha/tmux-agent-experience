// Package ownedfs binds tmag-owned directories to their inodes and performs
// operations relative to pinned directory handles. Symlinks, replaced ancestry
// and unreserved fixed artifact names preserve their objects.
package ownedfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrScope = errors.New("metadata directory ownership is uncertain")

type identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func id(st os.FileInfo) identity {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return identity{uint64(s.Dev), s.Ino} //nolint:unconvert // Darwin Dev is int32; Linux uses uint64.
	}
	return identity{}
}

func Owner(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
}

type manifest struct {
	Version   int                 `json:"version"`
	Root      identity            `json:"root"`
	Dirs      map[string]identity `json:"directories"`
	Slots     map[string]int      `json:"slot_namespaces,omitempty"`
	Temporary string              `json:"manifest_temporary,omitempty"`
}

type link struct {
	parent *os.Root
	name   string
	info   os.FileInfo
}

// Dir pins a directory and every ancestor within the selected state root.
type Dir struct {
	base  string
	info  os.FileInfo
	roots []*os.Root
	links []link
}

func component(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

func (d *Dir) current() *os.Root { return d.roots[len(d.roots)-1] }

func (d *Dir) Close() error {
	var err error
	for i := len(d.roots) - 1; i >= 0; i-- {
		err = errors.Join(err, d.roots[i].Close())
	}
	return err
}

// Validate checks the configured root and every literal child name against the
// pinned inode chain. Mutations still use handles, so a later alias cannot
// redirect the syscall into its target.
func (d *Dir) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := os.Lstat(d.base)
	if err != nil {
		return err
	}
	if !st.IsDir() || !Owner(st) || !os.SameFile(d.info, st) {
		return ErrScope
	}
	for _, l := range d.links {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := l.parent.Lstat(l.name)
		if err != nil {
			return err
		}
		if !st.IsDir() || !Owner(st) || !os.SameFile(l.info, st) {
			return ErrScope
		}
	}
	return nil
}

// Open pins rel beneath base. New children are made one component at a time.
// Existing bindings never silently rebind to a replacement inode.
func Open(ctx context.Context, base, rel string, create bool) (*Dir, error) {
	return open(ctx, base, rel, create, "", 0)
}

// OpenSlots reserves a fixed basename prefix in a pinned directory. A new
// reservation is admitted only when every position is absent; existing unknown
// objects never become owned merely because their basename resembles a slot.
// Cleanup passes create=false and cannot manufacture ownership evidence.
func OpenSlots(ctx context.Context, base, rel, prefix string, slots int, create bool) (*Dir, error) {
	if !component(prefix) || slots < 1 || slots > 256 {
		return nil, ErrScope
	}
	return open(ctx, base, rel, create, prefix, slots)
}

func open(ctx context.Context, base, rel string, create bool, prefix string, slots int) (_ *Dir, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := os.Lstat(base)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || !Owner(st) {
		return nil, ErrScope
	}
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	d := &Dir{base: base, info: st, roots: []*os.Root{r}}
	defer func() {
		if err != nil {
			_ = d.Close()
		}
	}()
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		return nil, ErrScope
	}
	gate, err := d.open(ctx, ".tmag-scope-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := syscall.Flock(int(gate.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	current, err := d.Stat(ctx, ".tmag-scope-lock")
	if err != nil {
		return nil, err
	}
	held, err := gate.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(current, held) {
		return nil, ErrScope
	}
	m, err := d.readManifest(ctx)
	if err != nil {
		return nil, err
	}
	changed := false
	if rel != "." && rel != "" {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		prefix := ""
		for _, name := range parts {
			if !component(name) {
				return nil, ErrScope
			}
			if err := d.Validate(ctx); err != nil {
				return nil, err
			}
			parent := d.current()
			info, err := parent.Lstat(name)
			if errors.Is(err, os.ErrNotExist) && create {
				err = parent.Mkdir(name, 0o700)
				if err != nil && !errors.Is(err, os.ErrExist) {
					return nil, err
				}
				info, err = parent.Lstat(name)
			}
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || !Owner(info) {
				return nil, ErrScope
			}
			child, err := parent.OpenRoot(name)
			if err != nil {
				return nil, err
			}
			d.roots = append(d.roots, child)
			d.links = append(d.links, link{parent, name, info})
			opened, err := child.Stat(".")
			if err != nil || !os.SameFile(info, opened) {
				return nil, ErrScope
			}
			if prefix != "" {
				prefix += "/"
			}
			prefix += name
			if expected, ok := m.Dirs[prefix]; ok {
				if expected != id(info) {
					return nil, ErrScope
				}
			} else {
				m.Dirs[prefix] = id(info)
				changed = true
			}
		}
	}
	if err := d.Validate(ctx); err != nil {
		return nil, err
	}
	if prefix != "" {
		key := filepath.ToSlash(filepath.Join(rel, prefix))
		if n, ok := m.Slots[key]; ok {
			if n != slots {
				return nil, ErrScope
			}
		} else {
			if !create {
				return nil, ErrScope
			}
			for slot := range slots {
				if err := d.Validate(ctx); err != nil {
					return nil, err
				}
				_, err := d.current().Lstat(fmt.Sprintf("%s%02x", prefix, slot))
				if !errors.Is(err, os.ErrNotExist) {
					return nil, ErrScope
				}
			}
			m.Slots[key] = slots
			changed = true
		}
	}
	if changed {
		if err := d.saveManifest(ctx, m); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *Dir) readManifest(ctx context.Context) (manifest, error) {
	var m manifest
	root := &Dir{base: d.base, info: d.info, roots: d.roots[:1]}
	f, err := root.open(ctx, ".tmag-scope.json", os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{Version: 1, Root: id(d.info), Dirs: map[string]identity{}, Slots: map[string]int{}}, nil
	}
	if err != nil {
		return m, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return m, err
	}
	if len(b) > 64<<10 || json.Unmarshal(b, &m) != nil || m.Version != 1 || m.Root != id(d.info) || m.Dirs == nil || len(m.Dirs) > 1024 {
		return m, ErrScope
	}
	if m.Temporary != "" && !validManifestTemporary(m.Temporary) {
		return m, ErrScope
	}
	if m.Slots == nil {
		m.Slots = map[string]int{}
	}
	return m, ctx.Err()
}

func (d *Dir) saveManifest(ctx context.Context, m manifest) error {
	r := &Dir{base: d.base, info: d.info, roots: d.roots[:1]}
	f, tmp, err := r.manifestTemporary(ctx, m.Temporary)
	if err != nil {
		return err
	}
	defer f.Close()
	m.Temporary = tmp
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > 64<<10 {
		return ErrScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return r.Rename(ctx, tmp, r, ".tmag-scope.json", st)
}

func (d *Dir) open(ctx context.Context, name string, flags int, perm os.FileMode) (*os.File, error) {
	return d.openUsing(ctx, name, flags, perm, d.current().OpenFile)
}

// openUsing keeps creation separate from opening an observed existing file.
// Root.OpenFile resolves symlinks even with O_NOFOLLOW; only exclusive creation
// can safely retain O_CREATE. The opener boundary also allows controlled races
// against the real Root.OpenFile implementation in regression tests.
func (d *Dir) openUsing(ctx context.Context, name string, flags int, perm os.FileMode, opener func(string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	if !component(name) {
		return nil, ErrScope
	}
	if flags&os.O_TRUNC != 0 {
		return nil, ErrScope
	}
	create := flags&os.O_CREATE != 0
	exclusive := create && flags&os.O_EXCL != 0
	for {
		if err := d.Validate(ctx); err != nil {
			return nil, err
		}
		before, statErr := d.current().Lstat(name)
		if statErr == nil && (!before.Mode().IsRegular() || !Owner(before) || before.Sys().(*syscall.Stat_t).Nlink != 1) {
			return nil, ErrScope
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		missing := errors.Is(statErr, os.ErrNotExist)
		openFlags := flags
		if !missing {
			if exclusive {
				return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
			}
			openFlags &^= os.O_CREATE | os.O_EXCL
		} else if create {
			openFlags |= os.O_EXCL
		}
		f, err := opener(name, openFlags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, perm)
		if create && !exclusive && ((missing && errors.Is(err, os.ErrExist)) || (!missing && errors.Is(err, os.ErrNotExist))) {
			// Disappearance retries with exclusive creation; a competing creator
			// retries with a non-creating open. Every iteration revalidates scope.
			continue
		}
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err == nil && (!opened.Mode().IsRegular() || !Owner(opened) || opened.Sys().(*syscall.Stat_t).Nlink != 1) {
			err = ErrScope
		}
		if err == nil && statErr == nil && !os.SameFile(before, opened) {
			err = ErrScope
		}
		after, afterErr := d.current().Lstat(name)
		if err == nil && (afterErr != nil || !after.Mode().IsRegular() || !os.SameFile(after, opened)) {
			err = ErrScope
		}
		if err == nil {
			err = d.Validate(ctx)
		}
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
}

func (d *Dir) OpenFile(ctx context.Context, name string, flags int, perm os.FileMode) (*os.File, error) {
	return d.open(ctx, name, flags, perm)
}

func (d *Dir) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if !component(name) {
		return nil, ErrScope
	}
	if err := d.Validate(ctx); err != nil {
		return nil, err
	}
	return d.current().Lstat(name)
}

func (d *Dir) Remove(ctx context.Context, name string, expected os.FileInfo) error {
	st, err := d.Stat(ctx, name)
	if err != nil {
		return err
	}
	if st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !Owner(st) || st.Sys().(*syscall.Stat_t).Nlink != 1 || !os.SameFile(expected, st) {
		return ErrScope
	}
	return d.current().Remove(name)
}

func (d *Dir) File(ctx context.Context) (*os.File, error) {
	if err := d.Validate(ctx); err != nil {
		return nil, err
	}
	return d.current().Open(".")
}

func (d *Dir) Rename(ctx context.Context, name string, to *Dir, target string, expected os.FileInfo) error {
	st, err := d.Stat(ctx, name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || !Owner(st) || !os.SameFile(st, expected) || !component(target) {
		return ErrScope
	}
	if err := to.Validate(ctx); err != nil {
		return err
	}
	if dst, err := to.current().Lstat(target); err == nil {
		if !dst.Mode().IsRegular() || !Owner(dst) || dst.Sys().(*syscall.Stat_t).Nlink != 1 {
			return ErrScope
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := d.Validate(ctx); err != nil {
		return err
	}
	if err := to.Validate(ctx); err != nil {
		return err
	}
	// Both names must be direct children of the same pinned directory. A
	// common-root rename of relative child paths would resolve replaceable
	// ancestors again and could reach an unrelated object inside that root.
	a, err := d.current().Stat(".")
	if err != nil {
		return err
	}
	b, err := to.current().Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return ErrScope
	}
	if err := d.current().Rename(name, target); err != nil {
		return fmt.Errorf("rename owned metadata: %w", err)
	}
	return nil
}
