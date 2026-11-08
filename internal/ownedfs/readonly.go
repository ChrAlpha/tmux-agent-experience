package ownedfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// OpenKnown pins only previously recorded directory bindings. Unlike Open it
// creates no gate, manifest, reservation or directory, and takes no scope lock.
// Atomic manifest reads and inode validation suffice for diagnostic evidence;
// unknown/replaced ancestry is refused rather than adopted by a reader.
func OpenKnown(ctx context.Context, base, rel string) (_ *Dir, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, err
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
	m, err := d.readManifestMode(ctx, false)
	if err != nil {
		return nil, err
	}
	if rel != "" && rel != "." {
		prefix := ""
		for _, name := range strings.Split(filepath.ToSlash(rel), "/") {
			if !component(name) {
				return nil, ErrScope
			}
			if prefix != "" {
				prefix += "/"
			}
			prefix += name
			if err := d.Validate(ctx); err != nil {
				return nil, err
			}
			parent := d.current()
			info, err := parent.Lstat(name)
			if err != nil {
				return nil, err
			}
			expected, known := m.Dirs[prefix]
			if !known {
				return nil, ErrScope
			}
			if !info.IsDir() || !Owner(info) || id(info) != expected {
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
