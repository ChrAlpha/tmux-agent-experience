package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
)

// A gitView is a repository as git reads it for tmag: through a git
// directory of tmag's own, made for one read and removed after it. Its
// configuration holds only the repository's settings that say how git reads
// files, and names no program; it borrows the repository's objects and
// index, read only, and keeps the objects tmag hashes from the work tree. No
// configuration of the repository's, the person's or the system's reaches
// git through it, so none of the programs they may name runs: no filter,
// diff driver, text conversion, file-system monitor or hook. Line ends still
// convert as the repository says; a file whose attributes name a filter is
// not compared, and said to have one.
type gitView struct {
	top      string // the work tree
	dir      string // the view's git directory
	index    string // the repository's index
	base     string // HEAD's commit, or the empty tree before a first commit
	empty    string // the empty tree
	filemode bool   // whether git counts a file's executable bit
	symlinks bool   // whether the work tree holds symbolic links as links
}

// emptyTrees name the empty tree in each object format.
var emptyTrees = map[string]string{
	"sha1":   "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
	"sha256": "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321",
}

// maxGitOutput bounds what tmag reads of git's status or a patch; maxFile
// bounds a file tmag reads from a work tree, and maxRead all the files one
// list reads.
const (
	maxGitOutput = maxPatch + 4096
	maxFile      = 8 << 20
	maxRead      = 64 << 20
)

// openView makes the view of the repository dir is in; close removes it.
func openView(ctx context.Context, dir string) (*gitView, error) {
	out, err := repoGit(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir", "--git-path", "index", "--show-object-format")
	f := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if err != nil || len(f) != 5 {
		return nil, fmt.Errorf("%w: %s", ErrNoRepository, dir)
	}
	top, gitDir, common, index, format := f[0], f[1], f[2], f[3], f[4]
	empty, ok := emptyTrees[format]
	if !ok || strings.Contains(common, "\n") {
		return nil, fmt.Errorf("%w: %s, in object format %s", ErrNoRepository, dir, format)
	}
	config, values, err := settings(ctx, top, format)
	if err != nil {
		return nil, err
	}
	g := &gitView{top: top, index: index, base: empty, empty: empty, filemode: values["filemode"] != "false", symlinks: values["symlinks"] != "false"}
	head := "ref: refs/heads/tmag\n"
	if out, err := repoGit(ctx, top, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		g.base = strings.TrimSpace(out)
		head = g.base + "\n"
	}
	if g.dir, err = os.MkdirTemp("", "tmag-git-"); err != nil {
		return nil, err
	}
	if err := g.lay(gitDir, common, head, config); err != nil {
		g.close()
		return nil, err
	}
	return g, nil
}

// lay writes the view's git directory: its HEAD, its configuration, the
// repository's objects as its alternate, and the repository's own files of
// ignored names and of attributes, whose rules the view must keep: one that
// is there and cannot be read whole fails the view. A split index finds its
// shared part there too.
func (g *gitView) lay(gitDir, common, head, config string) error {
	for _, d := range []string{"objects/info", "refs", "info"} {
		if err := os.MkdirAll(filepath.Join(g.dir, d), 0o700); err != nil {
			return err
		}
	}
	files := map[string]string{
		"HEAD":                    head,
		"config":                  config,
		"objects/info/alternates": filepath.Join(common, "objects") + "\n",
	}
	for _, name := range []string{"info/exclude", "info/attributes"} {
		b, err := readMetadata(filepath.Join(common, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		default:
			files[name] = string(b)
		}
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(g.dir, name), []byte(text), 0o600); err != nil {
			return err
		}
	}
	for _, d := range []string{gitDir, common} {
		shared, _ := filepath.Glob(filepath.Join(d, "sharedindex.*"))
		for _, s := range shared {
			if err := os.Symlink(s, filepath.Join(g.dir, filepath.Base(s))); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
	}
	return nil
}

// close removes the view's git directory and what it kept.
func (g *gitView) close() { _ = os.RemoveAll(g.dir) }

// readMetadata reads a regular file of at most maxPatch bytes, opened so
// that no FIFO holds it up.
func readMetadata(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	unread := fmt.Errorf("git: %s is no regular file of at most 1 MiB", path)
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPatch {
		return nil, unread
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPatch+1))
	if err == nil && len(b) > maxPatch {
		err = unread
	}
	return b, err
}

// settings is the view's configuration: the repository's object format and,
// from the repository's configuration as git resolves it, the settings that
// say how git reads the work tree, none of which names a program: whether
// file modes, symbolic links and case count, how line ends convert, which
// stat fields count, and the person's files of ignored names and of
// attributes. A value git would not take is left out. It returns the
// settings it took too.
func settings(ctx context.Context, top, format string) (string, map[string]string, error) {
	out, err := repoGit(ctx, top, "config", "-z", "--get-regexp", `^core\.(filemode|symlinks|ignorecase|precomposeunicode|trustctime|checkstat|autocrlf|eol)$`)
	if err != nil && exitCode(err) != 1 {
		return "", nil, err
	}
	values := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		key, value, given := strings.Cut(rec, "\n")
		if !given {
			value = "true" // a key alone is true
		}
		key = strings.TrimPrefix(key, "core.")
		if v, ok := setting(key, value); ok {
			values[key] = v
		}
	}
	lines := maps.Clone(values)
	for _, key := range []string{"excludesFile", "attributesFile"} {
		path, err := repoGit(ctx, top, "config", "-z", "--type=path", "--get", "core."+key)
		if err != nil && exitCode(err) != 1 {
			return "", nil, err
		}
		if path = strings.TrimSuffix(path, "\x00"); path != "" {
			if lines[key], err = quoted(path); err != nil {
				return "", nil, fmt.Errorf("git: core.%s: %w", key, err)
			}
		}
	}
	var b strings.Builder
	version := "0"
	if format != "sha1" {
		version = "1"
	}
	fmt.Fprintf(&b, "[core]\n\trepositoryformatversion = %s\n\tbare = false\n", version)
	for _, key := range slices.Sorted(maps.Keys(lines)) {
		fmt.Fprintf(&b, "\t%s = %s\n", key, lines[key])
	}
	if format != "sha1" {
		fmt.Fprintf(&b, "[extensions]\n\tobjectFormat = %s\n", format)
	}
	return b.String(), values, nil
}

// quoted is value as a git configuration file gives it, quoted, byte for
// byte as a path is, or an error for a control character git reads no
// escape for.
func quoted(value string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(value) {
		switch c := value[i]; {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c < ' ' || c == 0x7f:
			return "", fmt.Errorf("%q has a character a configuration cannot hold", value)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// setting is the value the view's configuration takes for one of the
// repository's settings, as git reads it.
func setting(key, value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	boolean := func() (string, bool) {
		switch value {
		case "true", "yes", "on", "1":
			return "true", true
		case "false", "no", "off", "0", "":
			return "false", true
		}
		return "", false
	}
	switch key {
	case "filemode", "symlinks", "ignorecase", "precomposeunicode", "trustctime":
		return boolean()
	case "autocrlf":
		if value == "input" {
			return value, true
		}
		return boolean()
	case "eol":
		return value, value == "lf" || value == "crlf" || value == "native"
	case "checkstat":
		return value, value == "default" || value == "minimal"
	}
	return "", false
}

// gitEnviron is the caller's environment without any GIT_ variable of its,
// with git writing no optional index update, reading pathspecs literally and
// fetching no object it lacks.
func gitEnviron() []string {
	return append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }),
		"GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_NO_LAZY_FETCH=1")
}

// repoGit runs git in dir on the repository as git finds it, for what reads
// only names and settings and so runs nothing a configuration names.
func repoGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, cut, err := runGit(ctx, dir, gitEnviron(), nil, maxGitOutput, args...)
	if err == nil && cut {
		err = fmt.Errorf("git %s printed more than %d bytes", args[0], maxGitOutput)
	}
	return out, err
}

// run runs git on the view, with index as its index and stdin as its input,
// and returns the first maxGitOutput bytes it printed, and whether it was
// cut.
func (g *gitView) run(ctx context.Context, index string, stdin io.Reader, args ...string) (string, bool, error) {
	return runGit(ctx, g.top, g.env(index), stdin, maxGitOutput, args...)
}

// complete runs git on the view for what it prints of paths, given on its
// input separated by NULs: all of it, within a bound their names set, or an
// error.
func (g *gitView) complete(ctx context.Context, index string, paths []string, args ...string) (string, error) {
	var in bytes.Buffer
	limit := 4096
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
		limit += 2*len(p) + 256
	}
	out, cut, err := runGit(ctx, g.top, g.env(index), &in, limit, args...)
	if err == nil && cut {
		err = fmt.Errorf("git %s printed more than %d bytes", args[0], limit)
	}
	return out, err
}

func (g *gitView) env(index string) []string {
	return append(gitEnviron(), "GIT_DIR="+g.dir, "GIT_WORK_TREE="+g.top, "GIT_INDEX_FILE="+index,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1")
}

// runGit runs git in dir with env and stdin, and returns what it printed,
// the first limit bytes of it, and whether it was cut; a failure says what
// git said first.
func runGit(ctx context.Context, dir string, env []string, stdin io.Reader, limit int, args ...string) (string, bool, error) {
	cmd := helper.Command(ctx, "git", append([]string{"--no-pager", "-c", "core.quotepath=off"}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, stdin
	var out, stderr bytes.Buffer
	w := &limited{w: &out, n: limit}
	cmd.Stdout, cmd.Stderr = w, &limited{w: &stderr, n: 512}
	if err := cmd.Run(); err != nil {
		said, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return out.String(), w.cut, fmt.Errorf("git %s: %w %s", args[0], err, said)
	}
	return out.String(), w.cut, nil
}

// records splits what git printed with -z into its complete records: what
// follows the last NUL is a record a reader cut, or nothing.
func records(out string) []string {
	r := strings.Split(out, "\x00")
	return r[:len(r)-1]
}

// status is git status of the work tree, or of the paths given: its
// complete records, at most maxChanged, and whether any were left out.
func (g *gitView) status(ctx context.Context, paths ...string) ([]Changed, bool, error) {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=dirty"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	out, cut, err := g.run(ctx, g.index, nil, args...)
	if err != nil {
		return nil, false, err
	}
	files, truncated := statusRecords(out, cut)
	return files, truncated, nil
}

// statusRecords reads git status --porcelain=v1 -z --no-renames, of which a
// reader kept only a part when cut: complete records only, at most
// maxChanged, and whether any were left out.
func statusRecords(out string, cut bool) ([]Changed, bool) {
	truncated := cut
	files := []Changed{}
	for _, r := range records(out) {
		if len(files) == maxChanged {
			truncated = true
			break
		}
		if len(r) < 4 || r[2] != ' ' {
			continue
		}
		files = append(files, Changed{Status: r[:2], Path: r[3:]})
	}
	return files, truncated
}

// gitEntry is a path's entry in an index or a tree: its mode and its
// object.
type gitEntry struct{ mode, name string }

// indexed is what the repository's index holds at each of paths, exactly:
// a path that names only a directory of entries has none.
func (g *gitView) indexed(ctx context.Context, paths []string) (map[string]gitEntry, error) {
	return g.listed(ctx, paths, "ls-files", "-s", "-z")
}

// inHEAD is what HEAD holds at each of paths, a directory's own entry
// included.
func (g *gitView) inHEAD(ctx context.Context, paths []string) (map[string]gitEntry, error) {
	if g.base == g.empty {
		return map[string]gitEntry{}, nil
	}
	return g.listed(ctx, paths, "ls-tree", "-z", g.base)
}

// listed runs git ls-files -s or ls-tree on paths, as many at a time as
// fit a command line, for their entries: all of them, or an error.
func (g *gitView) listed(ctx context.Context, paths []string, args ...string) (map[string]gitEntry, error) {
	got := map[string]gitEntry{}
	for len(paths) > 0 {
		n, size := 0, 0
		for n < len(paths) && (n == 0 || size+len(paths[n]) < 64<<10) {
			size += len(paths[n]) + 1
			n++
		}
		batch := paths[:n]
		paths = paths[n:]
		limit := maxGitOutput + 2*size
		out, cut, err := runGit(ctx, g.top, g.env(g.index), nil, limit, append(append(slices.Clone(args), "--"), batch...)...)
		if err == nil && cut {
			err = fmt.Errorf("git %s printed more than %d bytes", args[0], limit)
		}
		if err != nil {
			return nil, err
		}
		maps.Copy(got, entryRecords(out, batch))
	}
	return got, nil
}

// entryRecords reads git ls-files -s -z or ls-tree -z for paths: a mode,
// an object and a stage or type, a tab and the path, in complete records.
func entryRecords(out string, paths []string) map[string]gitEntry {
	got := map[string]gitEntry{}
	for _, rec := range records(out) {
		meta, path, ok := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && slices.Contains(paths, path) {
			name := f[1]
			if f[1] == "blob" || f[1] == "tree" || f[1] == "commit" {
				name = f[2] // ls-tree: mode, type, object
			}
			got[path] = gitEntry{mode: f[0], name: name}
		}
	}
	return got
}

// attributes is the value the attributes give each of paths for filter.
func (g *gitView) attributes(ctx context.Context, paths []string) (map[string]string, error) {
	out, err := g.complete(ctx, g.index, paths, "check-attr", "-z", "--stdin", "filter")
	if err != nil {
		return nil, err
	}
	return filterRecords(out), nil
}

// filterRecords reads git check-attr -z filter: each complete record of a
// path, its attribute and its value.
func filterRecords(out string) map[string]string {
	values := map[string]string{}
	f := records(out)
	for i := 0; i+2 < len(f); i += 3 {
		values[f[i]] = f[i+2]
	}
	return values
}

// filtered says whether an attribute's value names a filter: set, unset or
// unspecified names none.
func filtered(value string) bool {
	return value != "unspecified" && value != "unset" && value != "set"
}

// held is what a work tree holds at a path: a file's bytes and mode, a
// symbolic link's target, or nothing when mode is "". other is what tmag does
// not read, a directory or a device, and large a file or link past the bound
// it was read within; read is how many bytes reading it took.
type held struct {
	data  []byte
	mode  string
	other bool
	large bool
	read  int64
}

// readHeld reads path, of at most limit bytes, through root, opening each
// directory on the way without following a symbolic link, as git does: a
// path that goes through a link, or through a file, holds nothing. A path
// that changes from what it was while it is read is an error.
func readHeld(root *os.Root, path string, limit int64) (held, error) {
	dir, parts := root, strings.Split(path, "/")
	for _, name := range parts[:len(parts)-1] {
		fi, err := dir.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), err == nil && !fi.IsDir():
			return held{}, nil
		case err != nil:
			return held{}, err
		}
		sub, err := dir.OpenRoot(name)
		if err != nil {
			return held{}, err
		}
		defer sub.Close()
		if now, err := sub.Stat("."); err != nil || !os.SameFile(fi, now) {
			return held{}, fmt.Errorf("%s changed while it was read", path)
		}
		dir = sub
	}
	name := parts[len(parts)-1]
	fi, err := dir.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return held{}, nil
	case err != nil:
		return held{}, err
	case fi.Mode()&fs.ModeSymlink != 0 && fi.Size() > limit:
		return held{large: true}, nil // a link's size is its target's length
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := dir.Readlink(name)
		if int64(len(target)) > limit {
			return held{large: true, read: int64(len(target))}, err
		}
		return held{data: []byte(target), mode: "120000", read: int64(len(target))}, err
	case !fi.Mode().IsRegular():
		return held{other: true}, nil
	case fi.Size() > limit:
		return held{large: true}, nil
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return held{}, err
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !os.SameFile(fi, now) {
		return held{}, fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return held{read: int64(len(data))}, err
	}
	if int64(len(data)) > limit {
		return held{large: true, read: int64(len(data))}, nil
	}
	mode := "100644"
	if fi.Mode()&0o111 != 0 {
		mode = "100755"
	}
	return held{data: data, mode: mode, read: int64(len(data))}, nil
}

// hash writes what h holds at path into the view, as git would keep it
// there but for a filter, and returns its name. A link's target is kept as
// it is.
func (g *gitView) hash(ctx context.Context, path string, h held) (string, error) {
	args := []string{"hash-object", "-w", "--stdin", "--path=" + path}
	if h.mode == "120000" {
		args = []string{"hash-object", "-w", "--stdin", "--no-filters"}
	}
	out, _, err := g.run(ctx, g.index, bytes.NewReader(h.data), args...)
	return strings.TrimSpace(out), err
}

// hashAll hashes what each of held holds, a few at a time, by path; a path
// it could not hash has no name.
func (g *gitView) hashAll(ctx context.Context, held map[string]held) map[string]string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	names := map[string]string{}
	slots := make(chan struct{}, 8)
	for path, h := range held {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			if name, err := g.hash(ctx, path, h); err == nil {
				mu.Lock()
				names[path] = name
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return names
}

// entry is a path of a tree the view makes, its mode and its object; mode
// "0" leaves the path out.
type entry struct{ path, mode, name string }

// tree makes, in the view, a tree of entries: in place of what HEAD had at
// their paths, or alone. Paths left out go first, so a file may take the
// place of a directory and a directory that of a file.
func (g *gitView) tree(ctx context.Context, fromHEAD bool, entries []entry) (string, error) {
	index := filepath.Join(g.dir, "tmag-tree")
	read := []string{"read-tree", "--empty"}
	if fromHEAD && g.base != g.empty {
		read = []string{"read-tree", g.base}
	}
	if _, _, err := g.run(ctx, index, nil, read...); err != nil {
		return "", err
	}
	var in bytes.Buffer
	for _, out := range []bool{true, false} {
		for _, e := range entries {
			if (e.mode == "0") != out {
				continue
			}
			name := e.name
			if out {
				name = strings.Repeat("0", len(g.empty))
			}
			fmt.Fprintf(&in, "%s %s\t%s\x00", e.mode, name, e.path)
		}
	}
	if _, _, err := g.run(ctx, index, &in, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, _, err := g.run(ctx, index, nil, "write-tree")
	return strings.TrimSpace(out), err
}

// counts is what git counts between two trees, by path, for paths: the
// lines added and removed, -1 for a binary file. A path git printed nothing
// for did not change.
func (g *gitView) counts(ctx context.Context, from, to string, paths []string) (map[string][2]int, error) {
	out, err := g.complete(ctx, g.index, paths, "diff-tree", "-r", "-z", "--numstat", "--no-renames", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return nil, err
	}
	return numstatRecords(out), nil
}

// numstatRecords reads git diff-tree --numstat -z: complete records only.
func numstatRecords(out string) map[string][2]int {
	counts := map[string][2]int{}
	for _, rec := range records(out) {
		fields := strings.SplitN(rec, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, aerr := strconv.Atoi(fields[0])
		removed, rerr := strconv.Atoi(fields[1])
		if aerr != nil || rerr != nil {
			added, removed = -1, -1 // binary
		}
		counts[fields[2]] = [2]int{added, removed}
	}
	return counts
}

// exitCode is the exit status err reports, or -1.
func exitCode(err error) int {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// limited keeps the first n bytes written to it, and says whether it left
// any out.
type limited struct {
	w   *bytes.Buffer
	n   int
	cut bool
}

func (l *limited) Write(p []byte) (int, error) {
	room := l.n - l.w.Len()
	if len(p) > room {
		l.cut = true
	}
	if room > 0 {
		l.w.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
