package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only complete status records are files, and a list cut short says so: a
// record cut in the middle is no file git reported.
func TestStatusKeepsOnlyCompleteRecords(t *testing.T) {
	files, truncated := statusRecords(" M a.go\x00D  gone.go\x00?? notes.md\x00 M partial-na", true)
	if !truncated || len(files) != 3 || files[0] != (Changed{Path: "a.go", Status: " M"}) ||
		files[1] != (Changed{Path: "gone.go", Status: "D "}) || files[2] != (Changed{Path: "notes.md", Status: "??"}) {
		t.Fatalf("files = %+v, truncated %v", files, truncated)
	}
	if files, truncated := statusRecords(" M a.go\x00", false); truncated || len(files) != 1 {
		t.Fatalf("a whole list: %+v %v", files, truncated)
	}
	if files, truncated := statusRecords(strings.Repeat("?? f\x00", maxChanged+1), false); !truncated || len(files) != maxChanged {
		t.Fatalf("a list past its bound: %d files, truncated %v", len(files), truncated)
	}
}

// What git did not finish printing is nothing: a cut count never stands
// for another file's, an index entry is only the exact path's, and a cut
// attribute names no filter.
func TestRecordsAreOnlyCompleteOnes(t *testing.T) {
	if n := numstatRecords("1\t0\ta\x00-\t-\tlogo.png\x0099\t0\ta"); len(n) != 2 || n["a"] != [2]int{1, 0} || n["logo.png"] != [2]int{-1, -1} {
		t.Fatalf("counts = %v", n)
	}
	e := entryRecords("100644 aaaa 0\tdir/a\x00100755 bbbb 0\tx\x00160000 commit cccc\tsub\x00040000 tree eeee\ttree\x00100644 dddd 0\tcu", []string{"dir", "x", "sub", "tree", "cut"})
	if len(e) != 3 || e["x"] != (gitEntry{"100755", "bbbb"}) || e["sub"] != (gitEntry{"160000", "cccc"}) || e["tree"] != (gitEntry{"040000", "eeee"}) {
		t.Fatalf("entries = %v", e)
	}
	v := filterRecords("big.bin\x00filter\x00lfs\x00a.go\x00filter\x00unspecified\x00b.go\x00filter\x00set\x00c.go\x00filter\x00unset\x00d.go\x00filter\x00lf")
	if len(v) != 4 || v["big.bin"] != "lfs" || v["a.go"] != "unspecified" || !filtered(v["big.bin"]) || filtered(v["a.go"]) || filtered(v["b.go"]) || filtered(v["c.go"]) {
		t.Fatalf("attributes = %v", v)
	}
	if _, ok := v["d.go"]; ok {
		t.Fatalf("a cut record gave d.go %q", v["d.go"])
	}
}

// The view's configuration takes, of the repository's settings, only values
// git would take for how it reads files, and quotes the person's files of
// ignored names and attributes.
func TestSettingsTakeOnlyHowGitReadsFiles(t *testing.T) {
	for _, c := range []struct{ key, value, want string }{
		{"filemode", "No", "false"},
		{"ignorecase", "", "false"},
		{"autocrlf", "input", "input"},
		{"autocrlf", "1", "true"},
		{"eol", "CRLF", "crlf"},
		{"checkstat", "minimal", "minimal"},
	} {
		if got, ok := setting(c.key, c.value); !ok || got != c.want {
			t.Errorf("%s = %q: %q %v", c.key, c.value, got, ok)
		}
	}
	for _, c := range [][2]string{{"filemode", "maybe"}, {"eol", "cr"}, {"checkstat", "all"}, {"fsmonitor", "true"}, {"hookspath", "/x"}} {
		if got, ok := setting(c[0], c[1]); ok {
			t.Errorf("%s = %q taken as %q", c[0], c[1], got)
		}
	}
	dir := repo(t)
	git(t, dir, "config", "core.filemode", "false")
	git(t, dir, "config", "core.fsmonitor", "true")
	git(t, dir, "config", "core.excludesFile", `/tmp/a "b"\c`)
	git(t, dir, "config", "core.attributesFile", "/tmp/attributes")
	config, values, err := settings(context.Background(), dir, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	if values["filemode"] != "false" || !strings.HasPrefix(config, "[core]\n\trepositoryformatversion = 1\n\tbare = false\n") ||
		!strings.Contains(config, "\tfilemode = false\n") || strings.Contains(config, "fsmonitor") ||
		!strings.Contains(config, "\texcludesFile = \"/tmp/a \\\"b\\\"\\\\c\"\n") || !strings.Contains(config, "\tattributesFile = \"/tmp/attributes\"\n") ||
		!strings.HasSuffix(config, "[extensions]\n\tobjectFormat = sha256\n") {
		t.Fatalf("config %v:\n%s", values, config)
	}
}

// A work tree is read as git reads it, inside its root: a path through a
// symbolic link or through a file holds nothing, a link is its target, and
// a device, a directory or a file past its bound is not read.
func TestReadHeldReadsAsGitDoes(t *testing.T) {
	top, outside := t.TempDir(), t.TempDir()
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(outside, "secret.txt"), "outside\n", 0o644)
	write(filepath.Join(top, "a.txt"), "inside\n", 0o644)
	write(filepath.Join(top, "run.sh"), "#!/bin/sh\n", 0o755)
	write(filepath.Join(top, "sub", "f"), "below\n", 0o644)
	write(filepath.Join(top, "big.bin"), strings.Repeat("x", 4097), 0o644)
	for _, link := range [][2]string{{outside, "out"}, {"a.txt", "near"}, {"sub", "inner"}} {
		if err := os.Symlink(link[0], filepath.Join(top, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(top, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(top)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for path, want := range map[string]held{
		"a.txt":          {data: []byte("inside\n"), mode: "100644"},
		"run.sh":         {data: []byte("#!/bin/sh\n"), mode: "100755"},
		"sub/f":          {data: []byte("below\n"), mode: "100644"},
		"near":           {data: []byte("a.txt"), mode: "120000"},
		"out":            {data: []byte(outside), mode: "120000"},
		"out/secret.txt": {},
		"inner/f":        {},
		"a.txt/x":        {},
		"gone":           {},
		"pipe":           {other: true},
		"sub":            {other: true},
		"big.bin":        {large: true},
	} {
		h, err := readHeld(root, path, 4096)
		if err != nil || string(h.data) != string(want.data) || h.mode != want.mode || h.other != want.other || h.large != want.large {
			t.Errorf("%s: %+v %v", path, h, err)
		}
	}
}

// repo is a new repository in a directory of its own, git run with no
// configuration of the person's.
func repo(t *testing.T, args ...string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	git(t, dir, append([]string{"init", "-q"}, args...)...)
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func writeIn(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listed is what ListChanges lists for the repository in dir.
func listed(t *testing.T, dir string) (map[string]Changed, bool) {
	t.Helper()
	g, err := openView(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	files, truncated, err := g.status(context.Background())
	if err == nil {
		err = g.count(context.Background(), files)
	}
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Changed{}
	for _, f := range files {
		by[f.Status+" "+f.Path] = f
	}
	return by, truncated
}

// patched is what Diff gives for file of the repository in dir.
func patched(t *testing.T, dir, file string) (string, error) {
	t.Helper()
	g, err := openView(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	return g.patch(context.Background(), file)
}

func counted(f Changed) string {
	switch {
	case f.Filtered:
		return "filtered"
	case f.Binary:
		return "binary"
	case f.Added == nil:
		return "-"
	}
	return fmt.Sprintf("+%d -%d", *f.Added, *f.Removed)
}

// A file git ignores holds nothing, nor does a path through a symbolic
// link, even one that stays in the repository: a deletion git reports is
// no leave to read what lies there now.
func TestNoFileGitIgnoresIsRead(t *testing.T) {
	dir := repo(t)
	writeIn(t, dir, ".gitignore", "private/\n")
	writeIn(t, dir, "sub/a", "public\n")
	writeIn(t, dir, "a", "tracked\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	writeIn(t, dir, "private/a", "ignored marker\n")
	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "rm", "-q", "a")
	writeIn(t, dir, ".git/info/exclude", "a\n")
	writeIn(t, dir, "a", "ignored replacement\n")
	for _, file := range []string{"sub/a", "a"} {
		patch, err := patched(t, dir, file)
		if err != nil || !strings.Contains(patch, "deleted file mode") || strings.Contains(patch, "ignored") {
			t.Errorf("%s: %v\n%s", file, err, patch)
		}
	}
	by, _ := listed(t, dir)
	if counted(by[" D sub/a"]) != "+0 -1" || counted(by["D  a"]) != "+0 -1" {
		t.Fatalf("list: %+v", by)
	}
}

// A file's patch is that file's alone, where a file took a directory's
// place or a directory a file's; the list counts the place given up as
// deleted. The repository names its objects by SHA-256.
func TestAPatchIsItsFileAlone(t *testing.T) {
	dir := repo(t, "--object-format=sha256")
	writeIn(t, dir, "dir/a", "a\n")
	writeIn(t, dir, "dir/b", "b\n")
	writeIn(t, dir, "x", "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	if err := os.RemoveAll(filepath.Join(dir, "dir")); err != nil {
		t.Fatal(err)
	}
	writeIn(t, dir, "dir", "now a file\n")
	if err := os.Remove(filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	writeIn(t, dir, "x/a", "now below\n")
	for file, want := range map[string]string{"dir": "+now a file", "x/a": "+now below"} {
		patch, err := patched(t, dir, file)
		if err != nil || !strings.Contains(patch, want) || strings.Count(patch, "diff --git") != 1 {
			t.Errorf("%s: %v\n%s", file, err, patch)
		}
	}
	by, _ := listed(t, dir)
	for key, want := range map[string]string{" D dir/a": "+0 -1", " D x": "+0 -1", "?? dir": "+1 -0", "?? x/a": "+1 -0"} {
		if got := counted(by[key]); got != want {
			t.Errorf("%q counted %s, want %s (%+v)", key, got, want, by)
		}
	}
}

// Counts and filters are git's whole answer or none: names long enough to
// fill what tmag reads of a status leave every file counted, or filtered.
func TestCountsAreWholeOrNone(t *testing.T) {
	dir := repo(t)
	writeIn(t, dir, "keep", "keep\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	long := strings.Repeat("d", 200) + "/" + strings.Repeat("e", 200) + "/" + strings.Repeat("f", 200) + "/" + strings.Repeat("g", 200) + "/"
	if err := os.MkdirAll(filepath.Join(dir, long), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, long+strings.Repeat("h", 1048-len(long))), nil, 0o644); errors.Is(err, syscall.ENAMETOOLONG) {
		t.Skip("the file system takes no path this long, as macOS's does not")
	} else if err != nil {
		t.Fatal(err)
	} else if err := os.Remove(filepath.Join(dir, long+strings.Repeat("h", 1048-len(long)))); err != nil {
		t.Fatal(err)
	}
	for i := range maxChanged {
		name := fmt.Sprintf("%04d", i)
		writeIn(t, dir, long+name+strings.Repeat("h", 1048-len(long)-len(name)), "line\n")
	}
	by, truncated := listed(t, dir)
	if truncated || len(by) != maxChanged {
		t.Fatalf("%d files, truncated %v", len(by), truncated)
	}
	for key, f := range by {
		if counted(f) != "+1 -0" {
			t.Fatalf("%s counted %s", key[:12], counted(f))
		}
	}
	writeIn(t, dir, ".git/info/attributes", "* filter=review\n")
	by, _ = listed(t, dir)
	for key, f := range by {
		if !f.Filtered {
			t.Fatalf("%s is not filtered: %s", key[:12], counted(f))
		}
	}
}

// Where the work tree holds no symbolic links, a file stands for one git
// keeps as a link in its index, and where git counts no executable bit a
// file keeps its index entry's mode; a file git does not keep takes its
// mode as git adds a new one. A submodule is not compared, in the index or
// in HEAD, there or not.
func TestModesAndSubmodulesAreWhatGitKeeps(t *testing.T) {
	dir := repo(t)
	for _, name := range []string{"link", "gone-link"} {
		if err := os.Symlink("target", filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeIn(t, dir, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	for _, sub := range []string{"sub", "swapped"} {
		git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+","+sub)
	}
	git(t, dir, "commit", "-q", "-m", "submodules")
	git(t, dir, "config", "core.symlinks", "false")
	git(t, dir, "config", "core.filemode", "false")
	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	writeIn(t, dir, "link", "other")
	git(t, dir, "rm", "-q", "gone-link", "run.sh")
	writeIn(t, dir, "gone-link", "a file now")
	writeIn(t, dir, "run.sh", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "rm", "-q", "--cached", "swapped")
	writeIn(t, dir, "swapped", "a file now\n")
	git(t, dir, "add", "swapped")
	for file, want := range map[string][]string{
		"link":      {"-target\n", "+other\n"},
		"gone-link": {"deleted file mode 120000", "new file mode 100644"},
		"run.sh":    {"old mode 100755", "new mode 100644", "+exit 0"},
	} {
		patch, err := patched(t, dir, file)
		for _, w := range want {
			if err != nil || !strings.Contains(patch, w) {
				t.Errorf("%s lacks %q: %v\n%s", file, w, err, patch)
			}
		}
	}
	if patch, _ := patched(t, dir, "link"); strings.Contains(patch, "mode") {
		t.Errorf("an emulated link changed its mode:\n%s", patch)
	}
	for _, sub := range []string{"sub", "swapped"} {
		if _, err := patched(t, dir, sub); err == nil || !strings.Contains(err.Error(), sub+" is a submodule") {
			t.Errorf("%s's patch: %v", sub, err)
		}
	}
	by, _ := listed(t, dir)
	for key, want := range map[string]string{" M link": "+1 -1", " D sub": "-", "T  swapped": "-"} {
		if got := counted(by[key]); got != want {
			t.Errorf("%q counted %s, want %s (%+v)", key, got, want, by)
		}
	}
}

// A link's target counts against what a list reads, as a file's bytes do:
// past it, nothing more is read or compared.
func TestLinksCountAgainstWhatAListReads(t *testing.T) {
	dir := repo(t)
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	g, err := openView(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	sides, err := g.sides(context.Background(), []Changed{{Path: "link", Status: "??"}}, 0)
	if err != nil || sides["link"].compared {
		t.Fatalf("a link past the budget: %+v %v", sides["link"], err)
	}
	if sides, err := g.sides(context.Background(), []Changed{{Path: "link", Status: "??"}}, 6); err != nil || !sides["link"].compared {
		t.Fatalf("a link within the budget: %+v %v", sides["link"], err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if h, err := readHeld(root, "link", 5); err != nil || !h.large || h.read != 0 {
		t.Fatalf("a link past what is left is read: %+v %v", h, err)
	}
}

// The person's files of ignored names and attributes keep their rules
// whatever their paths hold: a newline or a tab is quoted as git reads it,
// and a path a configuration cannot hold fails the read.
func TestRuleFilesKeepTheirPaths(t *testing.T) {
	dir := repo(t)
	rules := t.TempDir()
	excludes, attributes := filepath.Join(rules, "rules\nignore"), filepath.Join(rules, "rules\tattributes")
	writeIn(t, rules, filepath.Base(excludes), "hidden.txt\n")
	writeIn(t, rules, filepath.Base(attributes), "*.bin filter=lfs\n")
	git(t, dir, "config", "core.excludesFile", excludes)
	git(t, dir, "config", "core.attributesFile", attributes)
	writeIn(t, dir, "hidden.txt", "ignored marker\n")
	writeIn(t, dir, "data.bin", "stored\n")
	if patch, err := patched(t, dir, "hidden.txt"); err == nil || !strings.Contains(err.Error(), "is no changed file") {
		t.Fatalf("an ignored file's patch: %v\n%s", err, patch)
	}
	if by, _ := listed(t, dir); !by["?? data.bin"].Filtered || len(by) != 1 {
		t.Fatalf("list: %+v", by)
	}
	for _, c := range [][2]string{{"a\nb\tc\bd\\e\"f", `"a\nb\tc\bd\\e\"f"`}} {
		if got, err := quoted(c[0]); err != nil || got != c[1] {
			t.Errorf("quoted(%q) = %s %v", c[0], got, err)
		}
	}
	// A path need not be UTF-8: its bytes are kept as they are, where the
	// file system takes such a name.
	raw := filepath.Join(rules, "rules-\xff")
	if err := os.WriteFile(raw, []byte("hidden.txt\n*.bin filter=lfs\n"), 0o644); err == nil {
		git(t, dir, "config", "core.excludesFile", raw)
		git(t, dir, "config", "core.attributesFile", raw)
		if patch, err := patched(t, dir, "hidden.txt"); err == nil {
			t.Fatalf("an ignored file's patch, its rules at a path that is not UTF-8:\n%s", patch)
		}
		if by, _ := listed(t, dir); !by["?? data.bin"].Filtered || len(by) != 1 {
			t.Fatalf("list, its rules at a path that is not UTF-8: %+v", by)
		}
	}
	git(t, dir, "config", "core.excludesFile", filepath.Join(rules, "rules\x01"))
	if _, err := openView(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "core.excludesFile") {
		t.Fatalf("a path a configuration cannot hold: %v", err)
	}
}

// The repository's own files of ignored names and attributes keep their
// rules or fail the read: one too large, or a FIFO, is no file, at once.
func TestRulesTheViewCannotKeepFailIt(t *testing.T) {
	dir := repo(t)
	writeIn(t, dir, ".git/info/exclude", "hidden.txt\n"+strings.Repeat("# filler\n", maxPatch/9+1))
	if _, err := openView(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "no regular file of at most 1 MiB") {
		t.Fatalf("an exclude file past its bound: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ".git", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ".git", "info", "exclude"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := openView(context.Background(), dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO for an exclude file opened a view")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO for an exclude file held the view up")
	}
}
