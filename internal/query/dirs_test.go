package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// A directory lists its subdirectories by name, links to directories among
// them, and marks those at a work tree's top; files are no subdirectories.
func TestADirectoryListsItsSubdirectories(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"b", "a/.git", ".hidden", "c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "c"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "file"), filepath.Join(root, "filelink")); err != nil {
		t.Fatal(err)
	}
	d, err := ListDir(context.Background(), root+"/")
	if err != nil {
		t.Fatal(err)
	}
	want := []Subdir{{Name: ".hidden"}, {Name: "a", Repo: true}, {Name: "b"}, {Name: "c"}, {Name: "link"}}
	if d.Path != root || d.Parent != filepath.Dir(root) || d.Repo != nil || d.Truncated || len(d.Dirs) != len(want) {
		t.Fatalf("dir = %+v", d)
	}
	for i, s := range want {
		if d.Dirs[i] != s {
			t.Fatalf("dirs[%d] = %+v, want %+v", i, d.Dirs[i], s)
		}
	}
	if d, err := ListDir(context.Background(), "/"); err != nil || d.Parent != "" {
		t.Fatalf("the root: %+v, %v", d, err)
	}
}

// Only an absolute path of a directory is listed: another is a usage error,
// and what names no directory says so.
func TestOnlyADirectoryIsListed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"relative/path", root + "/x/..", root + "/./file", root + "//file"} {
		if _, err := ListDir(context.Background(), p); !IsUsage(err) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "missing"), filepath.Join(root, "file"), filepath.Join(root, "pipe")} {
		if _, err := ListDir(context.Background(), p); !errors.Is(err, ErrNotDir) {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

// A directory in a repository names the work tree it is in, the main
// worktree and the branch, from a linked worktree too.
func TestADirectoryNamesItsRepository(t *testing.T) {
	main := t.TempDir()
	git(t, main, "init", "-q", "-b", "trunk")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "first")
	tree := filepath.Join(main, ".worktrees", "fix")
	git(t, main, "worktree", "add", "-q", "-b", "fix/login", tree)
	if err := os.MkdirAll(filepath.Join(tree, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	main, tree = resolved(main), resolved(tree)
	for dir, want := range map[string]Repo{
		main:                              {Top: main, Main: main, Branch: "trunk"},
		filepath.Join(main, ".git"):       {Top: main, Main: main, Branch: "trunk"},
		tree:                              {Top: tree, Main: main, Branch: "fix/login"},
		filepath.Join(tree, "src"):        {Top: tree, Main: main, Branch: "fix/login"},
		filepath.Join(main, ".worktrees"): {Top: main, Main: main, Branch: "trunk"},
	} {
		d, err := ListDir(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if d.Repo == nil || *d.Repo != want {
			t.Fatalf("%s: repo = %+v, want %+v", dir, d.Repo, want)
		}
	}
	git(t, tree, "checkout", "-q", "--detach")
	if r := repoOf(context.Background(), tree); r == nil || r.Branch != "" || r.Top != tree {
		t.Fatalf("a detached work tree: %+v", r)
	}
	if r := repoOf(context.Background(), t.TempDir()); r != nil {
		t.Fatalf("no repository: %+v", r)
	}
}

// Places are the directories sessions run in and events name, each once at
// its latest, latest first, as long as each is still a directory.
func TestPlacesAreWhereSessionsRanLately(t *testing.T) {
	root := t.TempDir()
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	for _, d := range []string{a, b, c} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := eventlog.Open(t.TempDir())
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, cwd := range []string{a, b, filepath.Join(root, "gone"), a, "relative", ""} {
		e := eventlog.Event{Event: "UserPromptSubmit", Agent: "claude", Session: "s", Cwd: cwd, Time: at.Add(time.Duration(i) * time.Minute)}
		if err := log.Append(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	since := at.Add(time.Hour)
	v := View{Sessions: []Session{{ID: "x", Agent: "codex", Cwd: c, Since: &since}}}
	p, err := ListPlaces(ctx, log, v, &attach.World{Proc: &proc.Table{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Recent) != 3 || p.Recent[0].Dir != c || p.Recent[1].Dir != a || p.Recent[2].Dir != b ||
		!p.Recent[1].At.Equal(at.Add(3*time.Minute)) || p.Templates == nil || len(p.Templates) != 0 {
		t.Fatalf("places = %+v", p)
	}
}

// A list of subdirectories is bounded, and so is what tmag reads to make it:
// either says the list stops short.
func TestAListOfSubdirectoriesIsBounded(t *testing.T) {
	many := t.TempDir()
	for i := range maxSubdirs + 1 {
		if err := os.Mkdir(filepath.Join(many, fmt.Sprintf("d%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := ListDir(context.Background(), many)
	if err != nil || !d.Truncated || len(d.Dirs) != maxSubdirs || d.Dirs[0].Name != "d0000" {
		t.Fatalf("subdirectories past the bound: %d, truncated %v, %v", len(d.Dirs), d.Truncated, err)
	}
	files := t.TempDir()
	for i := range maxScan {
		if err := os.WriteFile(filepath.Join(files, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if d, err := ListDir(context.Background(), files); err != nil || d.Truncated || len(d.Dirs) != 0 {
		t.Fatalf("entries up to the bound: %+v, %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(files, "more"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if d, err := ListDir(context.Background(), files); err != nil || !d.Truncated || len(d.Dirs) != 0 {
		t.Fatalf("entries past the bound: %+v, %v", d, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ListDir(ctx, files); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled read: %v", err)
	}
}
