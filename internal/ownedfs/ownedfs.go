// Package ownedfs operates within a dedicated tmag directory using pinned
// handles. Literal ancestry, effective ownership and inode identity are checked
// for each operation. Reserved names belong to tmag; symlinks are never adopted.
package ownedfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrScope = errors.New("metadata directory ownership is uncertain")

func Owner(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
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

// Open pins rel beneath the dedicated base directory. New children are made
// one literal component at a time. Runtime replacement refuses further work;
// a later invocation uses the operator-selected tree without a durable registry.
func Open(ctx context.Context, base, rel string, create bool) (_ *Dir, err error) {
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
	if rel != "." && rel != "" {
		parts := strings.Split(filepath.ToSlash(rel), "/")
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
		}
	}
	if err := d.Validate(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Dir) open(ctx context.Context, name string, flags int, perm os.FileMode) (*os.File, error) {
	return d.openUsing(ctx, name, flags, perm, d.current().OpenFile)
}

// unaliased refuses hard links. A reader may observe a published object while
// its successor replaces it: Linux drops the replaced inode's last link before
// the name moves, so both the name and the opened descriptor can report zero
// links, and the bytes are still one complete publication. Writers and lock
// holders need the object that stays at the name, so they need exactly one.
func unaliased(st os.FileInfo, read bool) bool {
	n := st.Sys().(*syscall.Stat_t).Nlink
	return n == 1 || read && n == 0
}

// openUsing keeps creation separate from opening an observed existing file.
// Root.OpenFile resolves symlinks even with O_NOFOLLOW; only exclusive creation
// can safely retain O_CREATE. A reader whose name moves to another object, or
// disappears, during the open observes it again; a writer refuses. The opener
// boundary also allows controlled races against the real Root.OpenFile
// implementation in regression tests.
func (d *Dir) openUsing(ctx context.Context, name string, flags int, perm os.FileMode, opener func(string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	if !component(name) {
		return nil, ErrScope
	}
	if flags&os.O_TRUNC != 0 {
		return nil, ErrScope
	}
	create := flags&os.O_CREATE != 0
	exclusive := create && flags&os.O_EXCL != 0
	read := !create && flags&(os.O_WRONLY|os.O_RDWR) == 0
	for {
		if err := d.Validate(ctx); err != nil {
			return nil, err
		}
		before, statErr := d.current().Lstat(name)
		if statErr == nil && (!before.Mode().IsRegular() || !Owner(before) || !unaliased(before, read)) {
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
		if err == nil && (!opened.Mode().IsRegular() || !Owner(opened) || !unaliased(opened, read)) {
			err = ErrScope
		}
		if err == nil {
			after, afterErr := d.current().Lstat(name)
			if (statErr == nil && !os.SameFile(before, opened)) || afterErr != nil || !os.SameFile(after, opened) {
				if read && (afterErr == nil || errors.Is(afterErr, os.ErrNotExist)) {
					_ = f.Close()
					continue
				}
				err = ErrScope
			}
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
