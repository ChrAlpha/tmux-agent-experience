package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Where an agent may start, for a client that cannot type a path blind:
// the directories sessions ran in lately, each with the repository it is
// in, the templates a start may run, and any directory's subdirectories.
// It is read from the log, the file system and git when asked, and kept
// nowhere; it reads no file's content, and runs no program a repository's
// configuration names.

// Repo is the git repository a directory is in: the top of the work tree
// it is in, the repository's main worktree, and the branch that work tree
// has checked out, "" when its HEAD is detached.
type Repo struct {
	Top    string `json:"top"`
	Main   string `json:"main,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// Place is a directory a session ran in, when one last did, and its
// repository, if it is in one.
type Place struct {
	Dir  string    `json:"dir"`
	At   time.Time `json:"at"`
	Repo *Repo     `json:"repo,omitempty"`
}

// Template is a start template the person set as @tmag-start-NAME, and the
// agent it runs.
type Template struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
}

// Places is where an agent may start: the person's home, the directories
// sessions ran in lately, latest first, and the templates of the tmux
// server tmag runs in.
type Places struct {
	Home      string     `json:"home,omitempty"`
	Recent    []Place    `json:"recent"`
	Templates []Template `json:"templates"`
}

// Subdir is a directory in another, and whether it holds a repository's
// work tree at its top.
type Subdir struct {
	Name string `json:"name"`
	Repo bool   `json:"repo,omitempty"`
}

// Dir is a directory: its parent, the repository it is in, and its
// subdirectories by name. Truncated says the list stops short of all of
// them.
type Dir struct {
	Path      string   `json:"path"`
	Parent    string   `json:"parent,omitempty"`
	Repo      *Repo    `json:"repo,omitempty"`
	Dirs      []Subdir `json:"dirs"`
	Truncated bool     `json:"truncated,omitempty"`
}

// maxPlaces bounds the directories Places lists, maxSubdirs those ListDir
// lists, maxScan the entries it reads of a directory, besides the one more
// that tells whether there are more, and maxTemplates the templates.
const (
	maxPlaces    = 40
	maxSubdirs   = 1000
	maxScan      = 10000
	maxTemplates = 64
)

// ListPlaces lists where an agent may start: the directories the sessions
// v shows run in and those the log's events name, each once, at the latest
// it was named, as long as it is still a directory; and the templates of
// the tmux server tmag runs in, when it runs in one.
func ListPlaces(ctx context.Context, log *eventlog.Log, v View, w *attach.World) (Places, error) {
	latest := map[string]time.Time{}
	note := func(dir string, at time.Time) {
		if filepath.IsAbs(dir) && at.After(latest[dir]) {
			latest[dir] = at
		}
	}
	events, err := log.Since(ctx, 0)
	if err != nil {
		return Places{}, err
	}
	for _, e := range events {
		note(e.Cwd, e.Time)
	}
	for _, s := range v.Sessions {
		at := time.Now()
		if s.Since != nil {
			at = *s.Since
		}
		note(s.Cwd, at)
	}
	dirs := make([]string, 0, len(latest))
	for d := range latest {
		dirs = append(dirs, d)
	}
	slices.SortFunc(dirs, func(a, b string) int {
		if c := latest[b].Compare(latest[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	p := Places{Recent: []Place{}, Templates: []Template{}}
	if home, err := os.UserHomeDir(); err == nil {
		p.Home = home
	}
	for _, d := range dirs {
		if len(p.Recent) == maxPlaces {
			break
		}
		if err := ctx.Err(); err != nil {
			return Places{}, err
		}
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			continue
		}
		p.Recent = append(p.Recent, Place{Dir: d, At: latest[d].UTC(), Repo: repoOf(ctx, d)})
	}
	if here, ok := w.Enclosing(os.Getpid()); ok {
		t, err := templates(ctx, tmux.Server{Socket: here.Server})
		if err != nil {
			return Places{}, err
		}
		p.Templates = t
	}
	return p, nil
}

// templateOption finds a template's option where tmux lists global options.
var templateOption = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(TemplateOption) + `([a-z0-9][a-z0-9_-]{0,31}) `)

// templates lists the start templates set on srv that run an agent, by name.
func templates(ctx context.Context, srv tmux.Server) ([]Template, error) {
	out, err := srv.Run(ctx, "show-options", "-g")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range templateOption.FindAllStringSubmatch(string(out), maxTemplates) {
		names = append(names, TemplateOption+m[1])
	}
	list := []Template{}
	if len(names) == 0 {
		return list, nil
	}
	values, err := srv.Options(ctx, names...)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		fields := strings.Fields(values[name])
		if len(fields) == 0 || !proc.IsAgent(filepath.Base(fields[0])) {
			continue
		}
		list = append(list, Template{Name: strings.TrimPrefix(name, TemplateOption), Agent: filepath.Base(fields[0])})
	}
	slices.SortFunc(list, func(a, b Template) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// ErrNotDir reports a path that names no directory tmag can list.
var ErrNotDir = errors.New("no directory")

// ListDir lists the subdirectories of dir, an absolute path in its shortest
// form, by name, and the repository dir is in. A subdirectory is one a link
// may name too. It reads at most maxScan of dir's entries, in the order the
// file system gives them, and one more to tell whether there are more, and
// lists at most maxSubdirs of them; it says so when it left any out.
func ListDir(ctx context.Context, dir string) (Dir, error) {
	if len(dir) > 1 {
		dir = strings.TrimRight(dir, "/")
	}
	// A path with . or .. elements may name another directory than its
	// shortest form does, through a link: tmag lists none it would not start in.
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return Dir{}, fmt.Errorf("%w: %q is no absolute path in its shortest form", errUsage, dir)
	}
	// Only a directory opens: a pipe, say, would wait for a writer.
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err == nil {
		defer f.Close()
	}
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return Dir{}, fmt.Errorf("%s is %w", dir, ErrNotDir)
	case err != nil:
		return Dir{}, err
	}
	d := Dir{Path: dir, Dirs: []Subdir{}}
	if parent := filepath.Dir(dir); parent != dir {
		d.Parent = parent
	}
	var names []string
	for scanned := 0; ; {
		if err := ctx.Err(); err != nil {
			return Dir{}, err
		}
		if scanned == maxScan {
			// One entry more says whether there were more than tmag read.
			more, err := f.ReadDir(1)
			if err != nil && !errors.Is(err, io.EOF) {
				return Dir{}, err
			}
			d.Truncated = len(more) > 0
			break
		}
		entries, err := f.ReadDir(min(256, maxScan-scanned))
		scanned += len(entries)
		for _, e := range entries {
			if !e.IsDir() {
				if e.Type()&os.ModeSymlink == 0 {
					continue
				}
				if info, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !info.IsDir() {
					continue
				}
			}
			names = append(names, e.Name())
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Dir{}, err
		}
	}
	slices.Sort(names)
	if len(names) > maxSubdirs {
		names, d.Truncated = names[:maxSubdirs], true
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Dir{}, err
		}
		_, err := os.Lstat(filepath.Join(dir, name, ".git"))
		d.Dirs = append(d.Dirs, Subdir{Name: name, Repo: err == nil})
	}
	d.Repo = repoOf(ctx, dir)
	return d, ctx.Err()
}

// repoOf is the repository dir is in, as git lists its worktrees, or nil.
func repoOf(ctx context.Context, dir string) *Repo {
	out, err := repoGit(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	trees := worktrees(out)
	if len(trees) == 0 {
		return nil
	}
	at := resolved(dir)
	var in *tree
	for i, t := range trees {
		p := resolved(t.path)
		if (at == p || strings.HasPrefix(at, p+string(filepath.Separator))) && (in == nil || len(p) > len(resolved(in.path))) {
			in = &trees[i]
		}
	}
	if in == nil {
		return nil
	}
	r := &Repo{Top: in.path, Branch: strings.TrimPrefix(in.branch, "refs/heads/")}
	if !trees[0].bare {
		r.Main = trees[0].path
	}
	return r
}

// resolved is p with its symbolic links resolved, where they exist.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
