package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// changes shows what an agent's directory changed against HEAD, and one
// file's patch, untracked files included; it keeps nothing of them.
func TestChangesShowWhatAnAgentsDirectoryChanged(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run(repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "parse.go"), []byte("package parse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".")
	run(repo, "commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "parse.go"), []byte("package parse\n\nfunc Parse() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("todo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := h.spawn(repo, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")

	if r := h.tmag("", "changes", sidA[:8]); r.code != 0 || r.out != resolved(t, repo)+"\n M parse.go (+2 -0)\n?? notes.md (+1 -0)\n" {
		t.Fatalf("changes: %d %q", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--json"); r.code != 0 || !strings.Contains(r.out, `{"path":"parse.go","status":" M","added":2,"removed":0}`) {
		t.Fatalf("changes --json: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "parse.go"); r.code != 0 || !strings.Contains(r.out, "+func Parse() {}") {
		t.Fatalf("a file's patch: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "notes.md"); r.code != 0 || !strings.Contains(r.out, "+todo") {
		t.Fatalf("an untracked file's patch: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "../outside"); r.code != 1 || !strings.Contains(r.out, "is no path in") {
		t.Fatalf("a file outside the repository: %d %s", r.code, r.out)
	}

	b := h.spawn(h.dir, map[string]string{"FAKE_SESSION": sidB, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(b, "idle", "process")
	if r := h.tmag("", "changes", sidB[:8]); r.code != 1 || !strings.Contains(r.out, "next: its directory is in no git repository") {
		t.Fatalf("changes outside a repository: %d %s", r.code, r.out)
	}
}

// git reads a repository for changes through a view of tmag's own, so
// nothing the repository's configuration names runs: no file-system
// monitor, diff driver, text conversion or filter, and nothing is cached in
// it. A file whose attributes name a filter is said to have one, with no
// count and no patch.
func TestChangesRunNothingARepositoryConfigures(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	marker := filepath.Join(h.dir, "ran")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	write("a.txt", "one\n")
	write("data.dat", "stored\n")
	run("add", ".")
	run("commit", "-q", "-m", "init")
	script := filepath.Join(h.dir, "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"core.fsmonitor", script},
		{"diff.external", script},
		{"diff.conv.textconv", script},
		{"diff.conv.cachetextconv", "true"},
		{"filter.evil.clean", script},
		{"filter.evil.smudge", script},
		{"filter.evil.process", script},
		{"filter.evil.required", "true"},
	} {
		run("config", kv[0], kv[1])
	}
	write(".gitattributes", "*.txt diff=conv\n*.dat filter=evil\n")
	write("a.txt", "two\n")
	write("data.dat", "changed\n")
	a := h.spawn(repo, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")
	if r := h.tmag("", "changes", sidA[:8], "--json"); r.code != 0 ||
		!strings.Contains(r.out, `{"path":"a.txt","status":" M","added":1,"removed":1}`) ||
		!strings.Contains(r.out, `{"path":"data.dat","status":" M","filtered":true}`) {
		t.Fatalf("changes --json: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "a.txt"); r.code != 0 || !strings.Contains(r.out, "-one\n+two\n") {
		t.Fatalf("a file's patch: %d %s", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "data.dat"); r.code != 1 || !strings.Contains(r.out, "through the filter evil, which tmag does not run") {
		t.Fatalf("a filtered file's patch: %d %s", r.code, r.out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("changes ran a program the repository configured")
	}
	if out, err := exec.Command("git", "-C", repo, "for-each-ref", "refs/notes/").Output(); err != nil || len(out) > 0 {
		t.Fatalf("changes cached a conversion: %v %s", err, out)
	}
}

// A file's patch is that file's change against HEAD, read inside the
// repository: no path through a symbolic link out of it, no pathspec magic,
// no directory, and no file git ignores; a file that did not change says
// so, and a repository with no commit yet still shows its files.
func TestChangesStayInsideTheRepository(t *testing.T) {
	h := start(t)
	repo, outside := filepath.Join(h.dir, "repo"), filepath.Join(h.dir, "outside")
	for _, d := range []string{filepath.Join(repo, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for path, text := range map[string]string{
		filepath.Join(outside, "secret.txt"): "outside the repository\n",
		filepath.Join(repo, "first.txt"):     "before any commit\n",
		filepath.Join(repo, "sub", "a.txt"):  "in a directory\n",
		filepath.Join(repo, ".gitignore"):    ".env\n",
		filepath.Join(repo, ".env"):          "TOKEN=outside the repository\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	a := h.spawn(repo, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")

	if r := h.tmag("", "changes", sidA[:8]); r.code != 0 || !strings.Contains(r.out, "?? first.txt (+1 -0)\n") || strings.Contains(r.out, ".env") {
		t.Fatalf("changes before a first commit: %d %q", r.code, r.out)
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "first.txt"); r.code != 0 || !strings.Contains(r.out, "+before any commit") {
		t.Fatalf("a file before a first commit: %d %s", r.code, r.out)
	}
	for file, want := range map[string]string{
		"link/secret.txt":         "is no changed file in",
		":(exclude)first.txt":     "is no changed file in",
		"sub":                     "is no changed file in",
		"missing.txt":             "is no changed file in",
		".env":                    "is no changed file in",
		"sub/../../outside/x.txt": "is no path in",
	} {
		r := h.tmag("", "changes", sidA[:8], "--file", file)
		if r.code != 1 || !strings.Contains(r.out, want) || strings.Contains(r.out, "outside the repository") {
			t.Errorf("--file %s: %d %s", file, r.code, r.out)
		}
	}
}

// A file's patch is its change against HEAD whatever the index says: a
// deletion staged or not, a file deleted from the index and written again,
// a file of any name; a directory deleted is no file.
func TestAFilesPatchIsItsChangeAgainstHEAD(t *testing.T) {
	h := start(t)
	repo := filepath.Join(h.dir, "repo")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	write("staged.txt", "staged gone\n")
	write("again.txt", "first\n")
	write("dir/1.txt", "one\n")
	write("dir/2.txt", "two\n")
	run("add", ".")
	run("commit", "-q", "-m", "init")
	run("rm", "-q", "staged.txt")
	run("rm", "-q", "--cached", "again.txt")
	write("again.txt", "second\n")
	if err := os.RemoveAll(filepath.Join(repo, "dir")); err != nil {
		t.Fatal(err)
	}
	write("--no-index", "new\n")
	a := h.spawn(repo, map[string]string{"FAKE_SESSION": sidA, "FAKE_HOOKS": "1"}, filepath.Join(bin, "claude"))
	h.waitPane(a, "idle", "process")

	if r := h.tmag("", "changes", sidA[:8]); r.code != 0 || !strings.Contains(r.out, "D  staged.txt (+0 -1)\n") ||
		!strings.Contains(r.out, " D dir/1.txt (+0 -1)\n") || !strings.Contains(r.out, "?? again.txt (+1 -1)\n") {
		t.Fatalf("changes: %d %q", r.code, r.out)
	}
	for file, want := range map[string]string{
		"staged.txt": "deleted file mode 100644",
		"again.txt":  "-first\n+second\n",
		"--no-index": "+new\n",
	} {
		if r := h.tmag("", "changes", sidA[:8], "--file", file); r.code != 0 || !strings.Contains(r.out, want) {
			t.Errorf("--file %s: %d %s", file, r.code, r.out)
		}
	}
	if r := h.tmag("", "changes", sidA[:8], "--file", "dir"); r.code != 1 || !strings.Contains(r.out, "is no changed file in") {
		t.Errorf("--file dir: %d %s", r.code, r.out)
	}
}
