package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Changed is a file the work tree of an agent's directory changed against
// HEAD: its path from the repository's top, how (git's two status letters,
// staged then unstaged, "??" for an untracked file), and the lines added
// and removed between HEAD and what the work tree holds, when tmag counted
// them: not for a binary file, a submodule, a file larger than maxFile or
// past maxRead in all, and not for a file whose attributes name a filter,
// which tmag does not run and says so.
type Changed struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	Added    *int   `json:"added,omitempty"`
	Removed  *int   `json:"removed,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	Filtered bool   `json:"filtered,omitempty"`
}

// Changes is what an agent's directory changed: the repository's top and
// its files, read from git when asked and kept nowhere. Truncated says the
// list stops short of all of them.
type Changes struct {
	Dir       string    `json:"dir"`
	Top       string    `json:"top"`
	Files     []Changed `json:"files"`
	Truncated bool      `json:"truncated,omitempty"`
}

// maxPatch bounds a file's patch as Diff prints it; maxChanged bounds how
// many files Changes lists.
const (
	maxPatch   = 1 << 20
	maxChanged = 1000
)

// ErrNoRepository reports a directory in no git repository.
var ErrNoRepository = errors.New("no git repository")

// ErrNoPatch reports a file tmag shows no patch of: no changed file of the
// repository's, a submodule, one too large, or one whose attributes name a
// filter.
var ErrNoPatch = errors.New("no patch")

// dirOf is the directory of the agent target names: its session's, or else
// its pane's.
func dirOf(ctx context.Context, v View, target string) (string, error) {
	t, err := Resolve(ctx, v, target)
	switch {
	case err != nil:
		return "", err
	case t.Session != nil && t.Session.Cwd != "":
		return t.Session.Cwd, nil
	case t.Pane != nil && t.Pane.Cwd != "":
		return t.Pane.Cwd, nil
	}
	return "", fmt.Errorf("%w: tmag knows no directory of %s", ErrNotFound, target)
}

// ListChanges lists what the work tree of target's directory changed
// against HEAD, untracked files included, as git status reports it, with
// the lines each added and removed as far as tmag counted them.
func ListChanges(ctx context.Context, v View, target string) (Changes, error) {
	dir, err := dirOf(ctx, v, target)
	if err != nil {
		return Changes{}, err
	}
	g, err := openView(ctx, dir)
	if err != nil {
		return Changes{}, err
	}
	defer g.close()
	files, truncated, err := g.status(ctx)
	if err != nil {
		return Changes{}, err
	}
	if err := g.count(ctx, files); err != nil {
		return Changes{}, err
	}
	return Changes{Dir: dir, Top: g.top, Files: files, Truncated: truncated}, nil
}

// count gives files the lines added and removed between HEAD and what the
// work tree holds, in one tree of what it holds at all of them.
func (g *gitView) count(ctx context.Context, files []Changed) error {
	if len(files) == 0 {
		return nil
	}
	sides, err := g.sides(ctx, files, maxRead)
	if err != nil {
		return err
	}
	var entries []entry
	var paths []string
	for p, s := range sides {
		if s.compared {
			entries = append(entries, s.entry)
			paths = append(paths, p)
		}
	}
	tree, err := g.tree(ctx, true, entries)
	if err != nil {
		return err
	}
	n, err := g.counts(ctx, g.base, tree, paths)
	if err != nil {
		return err
	}
	for i := range files {
		f := &files[i]
		s := sides[f.Path]
		c, counted := n[f.Path]
		switch {
		case s.filter != "":
			f.Filtered = true
		case !s.compared:
		case counted && c[0] < 0:
			f.Binary = true
		default: // not in git's count: HEAD has what the work tree holds
			f.Added, f.Removed = &c[0], &c[1]
		}
	}
	return nil
}

// side is what tmag compares of a path against HEAD: HEAD's entry, and the
// entry the work tree gives it, mode "0" when it gives none; or why tmag
// compares nothing.
type side struct {
	head     gitEntry
	entry    entry
	compared bool
	filter   string // the filter the attributes name
	why      string // why it compares nothing
}

// sides decides, for each path of files, what tmag compares, reading the
// work tree within budget. The work tree gives a path what it holds there
// only when git keeps the path in its index, or reports it untracked: a file
// git ignores holds nothing, nor does a path through a symbolic link, and a
// directory or device where git kept a file stands for its deletion. A
// submodule in the index or in HEAD, and a file whose attributes name a
// filter, are not compared. A file takes its mode as git adds it: where the
// work tree holds no symbolic links, a file whose index entry is one stands
// for it, and where git counts no executable bit, a file keeps its index
// entry's mode, or is not executable.
func (g *gitView) sides(ctx context.Context, files []Changed, budget int64) (map[string]*side, error) {
	var paths []string
	untracked := map[string]bool{}
	for _, f := range files {
		if !slices.Contains(paths, f.Path) {
			paths = append(paths, f.Path)
		}
		untracked[f.Path] = untracked[f.Path] || f.Status == "??"
	}
	// An untracked path is in no index: asking for it would list whatever
	// the index holds below it.
	tracked, err := g.indexed(ctx, slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return untracked[p] }))
	if err != nil {
		return nil, err
	}
	head, err := g.inHEAD(ctx, paths)
	if err != nil {
		return nil, err
	}
	attrs, err := g.attributes(ctx, paths)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(g.top)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	sides, toHash := map[string]*side{}, map[string]held{}
	for _, p := range paths {
		s := &side{head: head[p], entry: entry{path: p, mode: "0"}}
		sides[p] = s
		index, inIndex := tracked[p]
		known := index.mode
		if known == "" {
			known = head[p].mode
		}
		value, ok := attrs[p]
		switch {
		case index.mode == "160000" || head[p].mode == "160000":
			s.why = "is a submodule"
			continue
		case !ok:
			s.why = "has no attributes git reported"
			continue
		case filtered(value):
			s.filter, s.why = value, "is kept through the filter "+value+", which tmag does not run"
			continue
		}
		var h held
		limit := min(maxFile, budget)
		if inIndex || untracked[p] {
			h, err = readHeld(root, p, limit)
			budget = max(0, budget-h.read)
			if err != nil {
				s.why = "could not be read: " + err.Error()
				continue
			}
		}
		switch {
		case h.large:
			s.why = fmt.Sprintf("is larger than the %d bytes tmag reads", limit)
			continue
		case h.other && known == "":
			s.why = "is no file"
			continue
		case h.mode != "" && !h.other:
			regular := h.mode != "120000"
			switch {
			case regular && !g.symlinks && index.mode == "120000":
				h.mode = "120000"
			case regular && !g.filemode && (index.mode == "100644" || index.mode == "100755"):
				h.mode = index.mode
			case regular && !g.filemode:
				h.mode = "100644"
			}
			toHash[p] = h
		}
		s.compared = true
	}
	names := g.hashAll(ctx, toHash)
	for p, h := range toHash {
		if names[p] == "" {
			sides[p].compared, sides[p].why = false, "could not be hashed"
			continue
		}
		sides[p].entry = entry{path: p, mode: h.mode, name: names[p]}
	}
	return sides, nil
}

// Diff is the patch of one changed file of target's directory, between
// HEAD and what the work tree holds, cut at maxPatch: a file git status
// reports, named by its path from the repository's top, compared as
// ListChanges compares it and alone, so a patch shows no other path, nothing
// outside the repository and no file git ignores.
func Diff(ctx context.Context, v View, target, file string) (string, error) {
	dir, err := dirOf(ctx, v, target)
	if err != nil {
		return "", err
	}
	g, err := openView(ctx, dir)
	if err != nil {
		return "", err
	}
	defer g.close()
	return g.patch(ctx, file)
}

// patch is the patch of file, as Diff gives it.
func (g *gitView) patch(ctx context.Context, file string) (string, error) {
	rel := filepath.Clean(file)
	if filepath.IsAbs(file) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s is no path in %s", ErrNoPatch, file, g.top)
	}
	files, _, err := g.status(ctx, rel)
	if err != nil {
		return "", fmt.Errorf("%w: %s is no changed file in %s: %w", ErrNoPatch, file, g.top, err)
	}
	files = slices.DeleteFunc(files, func(f Changed) bool { return f.Path != rel })
	if len(files) == 0 {
		return "", fmt.Errorf("%w: %s is no changed file in %s", ErrNoPatch, file, g.top)
	}
	sides, err := g.sides(ctx, files, maxFile)
	if err != nil {
		return "", err
	}
	s := sides[rel]
	if !s.compared {
		return "", fmt.Errorf("%w: %s %s", ErrNoPatch, file, s.why)
	}
	var was, is []entry
	if m := s.head.mode; m == "100644" || m == "100755" || m == "120000" {
		was = []entry{{path: rel, mode: m, name: s.head.name}}
	}
	if s.entry.mode != "0" {
		is = []entry{s.entry}
	}
	from, err := g.tree(ctx, false, was)
	if err != nil {
		return "", err
	}
	to, err := g.tree(ctx, false, is)
	if err != nil {
		return "", err
	}
	patch, cut, err := g.run(ctx, g.index, nil, "diff-tree", "-p", "-r", "--no-renames", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return "", err
	}
	if cut || len(patch) > maxPatch {
		patch = patch[:min(len(patch), maxPatch)] + "\n[cut at 1 MiB]\n"
	}
	return patch, nil
}
