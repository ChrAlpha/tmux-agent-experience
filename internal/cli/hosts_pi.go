package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/binpath"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
)

// Pi and oh-my-pi install tmag's plugins as local packages from the tmux
// plugin's own directory (plugins/pi, plugins/omp), which pi install and
// omp plugin install take by path: a package always matches the binary its
// extension runs, updates with the tmux plugin, and leaves with it.

// pluginRoot is the directory of the tmux plugin whose tmag agent hooks run,
// "" when they run a tmag of no plugin.
func pluginRoot(getenv binpath.Getenv) string {
	found := hookBinaries(getenv)
	if len(found) == 0 {
		return ""
	}
	for _, bin := range []string{found[0], canonical(found[0])} {
		if inPluginDir(bin) {
			return filepath.Dir(filepath.Dir(bin))
		}
	}
	return ""
}

// packagesDir is where the agent installs tmag's plugins from: its
// directory in the tmux plugin whose tmag agent hooks run, when that plugin
// has one; otherwise "".
func packagesDir(getenv binpath.Getenv, agent string) string {
	root := pluginRoot(getenv)
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, "plugins", agent)
	if _, err := os.Stat(filepath.Join(dir, pluginName, "package.json")); err != nil {
		return ""
	}
	return dir
}

// packageVersion is the version a package directory declares, "" when it
// does not say.
func packageVersion(ctx context.Context, dir string) string {
	data, err := contextio.ReadFile(ctx, filepath.Join(dir, "package.json"), 1<<20)
	var pkg struct{ Version string }
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return hostMetadata(pkg.Version, 64)
}

// samePath reports whether two paths name one directory.
func samePath(a, b string) bool { return canonical(filepath.Clean(a)) == canonical(filepath.Clean(b)) }

// The version lines of Pi 1.5 ("1.5.0") and oh-my-pi 18.18 ("omp/18.18.6"):
// another program of the same name is no host of tmag's.
var (
	piVersionRE  = regexp.MustCompile(`^\d+\.\d+\.\d+`)
	ompVersionRE = regexp.MustCompile(`^omp/\d+\.\d+`)
)

// agentProbe probes the agent's CLI, which is present only when its version
// line is the agent's.
func agentProbe(ctx context.Context, h host, version *regexp.Regexp) host {
	h.Version, h.Present, h.Unavailable = probe(ctx, h.CLI)
	if h.Present && !h.Unavailable && !version.MatchString(h.Version) {
		return host{Name: h.Name, CLI: h.CLI}
	}
	return h
}

func piHostContext(ctx context.Context) host {
	h := agentProbe(ctx, host{Name: "Pi", CLI: "pi"}, piVersionRE)
	if !h.Present || h.Unavailable {
		return h
	}
	h.Packages = packagesDir(os.Getenv, "pi")
	packages, err := piPackages(ctx)
	if err != nil {
		h.Unavailable = true
		return h
	}
	h.Listed = true
	for _, name := range []string{pluginName, peersName} {
		p := h.of(name)
		for _, pkg := range packages {
			if !strings.HasSuffix(filepath.ToSlash(pkg.Path), "/plugins/pi/"+name) {
				continue
			}
			if h.Packages == "" || !samePath(pkg.Path, filepath.Join(h.Packages, name)) {
				p.Elsewhere = append(p.Elsewhere, pkg.Path)
				continue
			}
			filter := pkg.Extensions
			if name == peersName {
				filter = pkg.Skills
			}
			p.Installed, p.Enabled = true, piSelects(pkg.Path, piResources[name], filter)
			p.Version = packageVersion(ctx, pkg.Path)
		}
	}
	return h
}

// piResources are what Pi loads of tmag's packages, relative to them: the
// extension, and the peers skill.
var piResources = map[string]string{pluginName: "extensions/tmag.ts", peersName: "skills/peers/SKILL.md"}

// piSelects reports whether Pi 1.5 loads a package's resource file
// (relative to root) under patterns, a settings entry's filter of the
// resource's kind, nil when it has none: an empty filter loads nothing;
// otherwise globs include (all when none does), !globs exclude, +paths
// include again and -paths exclude again. A skill is also known by its
// directory.
func piSelects(root, file string, patterns *[]string) bool {
	if patterns == nil {
		return true
	}
	if len(*patterns) == 0 {
		return false
	}
	abs := filepath.ToSlash(filepath.Join(root, file))
	names, exact := []string{file, path.Base(file), abs}, []string{file, abs}
	if path.Base(file) == "SKILL.md" {
		names = append(names, path.Dir(file), path.Base(path.Dir(file)), path.Dir(abs))
		exact = append(exact, path.Dir(file), path.Dir(abs))
	}
	globs := func(pattern string) bool {
		re := globRE(strings.TrimPrefix(filepath.ToSlash(pattern), "./"))
		return slices.ContainsFunc(names, re.MatchString)
	}
	is := func(pattern string) bool {
		return slices.Contains(exact, strings.TrimPrefix(filepath.ToSlash(pattern), "./"))
	}
	var includes, excludes, again, never []string
	for _, p := range *patterns {
		switch {
		case strings.HasPrefix(p, "+"):
			again = append(again, p[1:])
		case strings.HasPrefix(p, "-"):
			never = append(never, p[1:])
		case strings.HasPrefix(p, "!"):
			excludes = append(excludes, p[1:])
		default:
			includes = append(includes, p)
		}
	}
	selected := (len(includes) == 0 || slices.ContainsFunc(includes, globs)) && !slices.ContainsFunc(excludes, globs)
	return (selected || slices.ContainsFunc(again, is)) && !slices.ContainsFunc(never, is)
}

// globRE matches a whole path against a glob as minimatch does without
// classes, braces or extglobs: ** crosses directories, * and ? do not.
func globRE(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// tmagPiPackages are the paths of tmag's packages Pi's settings list,
// tmag-peers first, as pi remove takes them.
func tmagPiPackages(ctx context.Context) []string {
	packages, _ := piPackages(ctx)
	var out []string
	for _, name := range []string{peersName, pluginName} {
		for _, pkg := range packages {
			if strings.HasSuffix(filepath.ToSlash(pkg.Path), "/plugins/pi/"+name) {
				out = append(out, pkg.Path)
			}
		}
	}
	return out
}

// piPackage is a local package Pi's settings declare.
type piPackage struct {
	Path string // absolute
	// Extensions and Skills are the object form's filters, nil when the
	// entry does not filter that kind of resource.
	Extensions, Skills *[]string
}

// piAgentDir is Pi's configuration directory.
func piAgentDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		abs, _ := filepath.Abs(dir)
		return abs
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// piPackages reads the local packages of Pi's user settings, where pi
// install writes them; it reads nothing else of the file.
func piPackages(ctx context.Context) ([]piPackage, error) {
	dir := piAgentDir()
	if dir == "" {
		return nil, errors.New("no home directory")
	}
	data, err := contextio.ReadFile(ctx, filepath.Join(dir, "settings.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settings struct {
		Packages []json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	var out []piPackage
	for _, raw := range settings.Packages {
		var entry struct {
			Source             string
			Extensions, Skills *[]string
		}
		if json.Unmarshal(raw, &entry.Source) != nil && json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if path := piLocalPath(dir, entry.Source); path != "" {
			out = append(out, piPackage{Path: path, Extensions: entry.Extensions, Skills: entry.Skills})
		}
	}
	return out, nil
}

// piLocalPath resolves a package source as Pi 1.5 does when it names a
// local directory: relative to the settings' directory, ~ for the home
// directory. Other sources (npm:, git: and URLs) resolve to "".
func piLocalPath(base, source string) string {
	source = strings.TrimSpace(source)
	for _, remote := range []string{"npm:", "git:", "github:", "http:", "https:", "ssh:", "builtin:"} {
		if strings.HasPrefix(source, remote) {
			return ""
		}
	}
	source = strings.TrimPrefix(source, "file://")
	if source == "~" || strings.HasPrefix(source, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		source = filepath.Join(home, strings.TrimPrefix(source, "~"))
	}
	if source == "" {
		return ""
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(base, source)
	}
	return filepath.Clean(source)
}

func ompHostContext(ctx context.Context) host {
	h := agentProbe(ctx, host{Name: "oh-my-pi", CLI: "omp"}, ompVersionRE)
	if !h.Present || h.Unavailable {
		return h
	}
	h.Packages = packagesDir(os.Getenv, "omp")
	out, err := run(ctx, "omp", "plugin", "list", "--json")
	var list struct {
		NPM []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Path    string `json:"path"`
			Enabled bool   `json:"enabled"`
		} `json:"npm"`
	}
	if err != nil || json.Unmarshal([]byte(out), &list) != nil {
		h.Unavailable = true
		return h
	}
	h.Listed = true
	for _, installed := range list.NPM {
		if installed.Name != pluginName && installed.Name != peersName {
			continue
		}
		p := h.of(installed.Name)
		// oh-my-pi links a local plugin under its name.
		if h.Packages == "" || !samePath(installed.Path, filepath.Join(h.Packages, installed.Name)) {
			p.Elsewhere = append(p.Elsewhere, installed.Name)
			continue
		}
		*p = plugin{Installed: true, Enabled: installed.Enabled, Version: hostMetadata(installed.Version, 64)}
	}
	return h
}
