package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/version"
)

// piWorld lays out a tmux plugin with tmag's packages for Pi and oh-my-pi,
// and a fake host CLI named agent, alone on PATH, that prints out (with no
// single quote) for every command but --version, which prints the agent's
// version line. It returns the plugin's directory.
func piWorld(t *testing.T, agent, out string) string {
	t.Helper()
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	tpm := t.TempDir()
	root := filepath.Join(tpm, "tmux-agent-experience")
	write(filepath.Join(root, "bin", "tmag"), "#!/bin/sh\n", 0o755)
	for _, host := range []string{"pi", "omp"} {
		for _, name := range []string{pluginName, peersName} {
			write(filepath.Join(root, "plugins", host, name, "package.json"), `{"name":"`+name+`","version":"`+version.Version+`"}`, 0o644)
		}
	}
	bin := t.TempDir()
	version := map[string]string{"pi": "1.5.0", "omp": "omp/18.18.6"}[agent]
	write(filepath.Join(bin, agent), "#!/bin/sh\ncase $1 in --version) echo "+version+";; *) printf '%s\\n' '"+out+"';; esac\n", 0o755)
	t.Setenv("TMUX_PLUGIN_MANAGER_PATH", tpm)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", bin)
	return root
}

// Pi's packages are those its settings list by a path to plugins/pi: the
// tmux plugin's own are installed, and enabled unless a filter loads none
// of their resources; any other copy is to be replaced.
func TestPiHostReadsItsSettings(t *testing.T) {
	root := piWorld(t, "pi", "")
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	old := filepath.Join(t.TempDir(), "plugins", "pi", "tmag")
	rel, err := filepath.Rel(agentDir, filepath.Join(root, "plugins", "pi", "tmag"))
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"theme":"dark","packages":["npm:@x/tmag","` + old + `","` + rel + `",` +
		`{"source":"` + filepath.Join(root, "plugins", "pi", "tmag-peers") + `","skills":[]}]}`
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	h := piHostContext(context.Background())
	if !h.Present || !h.Listed || h.Version != "1.5.0" || h.Packages != filepath.Join(root, "plugins", "pi") {
		t.Fatalf("host %+v", h)
	}
	want := plugin{Installed: true, Enabled: true, Version: version.Version, Elsewhere: []string{old}}
	if !reflect.DeepEqual(h.Tmag, want) {
		t.Errorf("tmag %+v, want %+v", h.Tmag, want)
	}
	if want := (plugin{Installed: true, Version: version.Version}); !reflect.DeepEqual(h.Peers, want) {
		t.Errorf("tmag-peers %+v, want %+v", h.Peers, want)
	}
}

// oh-my-pi lists its plugins by name and the path it links them under.
func TestOMPHostReadsItsPluginList(t *testing.T) {
	links := t.TempDir()
	list := `{"npm":[{"name":"tmag","version":"` + version.Version + `","path":"` + filepath.Join(links, "tmag") + `","enabled":false},` +
		`{"name":"tmag-peers","version":"0.0.1","path":"` + filepath.Join(links, "tmag-peers") + `","enabled":true},` +
		`{"name":"other","version":"1.0.0","path":"/x","enabled":true}],"marketplace":[]}`
	root := piWorld(t, "omp", list)
	if err := os.Symlink(filepath.Join(root, "plugins", "omp", "tmag"), filepath.Join(links, "tmag")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(links, "tmag-peers")); err != nil {
		t.Fatal(err)
	}
	h := ompHostContext(context.Background())
	if !h.Present || !h.Listed || h.Packages != filepath.Join(root, "plugins", "omp") {
		t.Fatalf("host %+v", h)
	}
	if want := (plugin{Installed: true, Version: version.Version}); !reflect.DeepEqual(h.Tmag, want) {
		t.Errorf("tmag %+v, want %+v", h.Tmag, want)
	}
	if want := (plugin{Elsewhere: []string{"tmag-peers"}}); !reflect.DeepEqual(h.Peers, want) {
		t.Errorf("tmag-peers %+v, want %+v", h.Peers, want)
	}
}

// Without a tmux plugin whose tmag hooks run, there is nothing to install
// Pi's packages from.
func TestPiPackagesComeWithTheTmuxPlugin(t *testing.T) {
	piWorld(t, "pi", "")
	t.Setenv("TMUX_PLUGIN_MANAGER_PATH", t.TempDir())
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	if h := piHostContext(context.Background()); !h.Listed || h.Packages != "" || setupPlan(h, true) != nil {
		t.Fatalf("host %+v plans %v", h, setupPlan(h, true))
	}
	for source, want := range map[string]string{
		"npm:@x/y": "", "git:github.com/x/y": "", "https://github.com/x/y": "",
		"../p": "/a/p", "/abs/p": "/abs/p", "file:///abs/p": "/abs/p",
	} {
		if got := piLocalPath("/a/agent", source); got != want {
			t.Errorf("piLocalPath(%q) = %q, want %q", source, got, want)
		}
	}
}

// Pi's filters select what its configuration loads of a package: pi config
// writes an exact exclusion to turn a resource off.
func TestPiSelects(t *testing.T) {
	root := "/p/plugins/pi/tmag"
	for _, c := range []struct {
		file     string
		patterns []string
		want     bool
	}{
		{"extensions/tmag.ts", []string{"-extensions/tmag.ts"}, false},
		{"extensions/tmag.ts", []string{"+extensions/tmag.ts"}, true},
		{"extensions/tmag.ts", []string{"!extensions/*.ts"}, false},
		{"extensions/tmag.ts", []string{"!extensions/*.ts", "+extensions/tmag.ts"}, true},
		{"extensions/tmag.ts", []string{"extensions/other.ts"}, false},
		{"extensions/tmag.ts", []string{"**/*.ts"}, true},
		{"extensions/tmag.ts", []string{"tmag.ts"}, true},
		{"extensions/tmag.ts", []string{"/p/plugins/pi/tmag/extensions/tmag.ts", "-./extensions/tmag.ts"}, false},
		{"skills/peers/SKILL.md", []string{"-skills/peers"}, false},
		{"skills/peers/SKILL.md", []string{"!peers"}, false},
		{"skills/peers/SKILL.md", []string{"skills/*"}, true},
	} {
		if got := piSelects(root, c.file, &c.patterns); got != c.want {
			t.Errorf("piSelects(%s, %q) = %v, want %v", c.file, c.patterns, got, c.want)
		}
	}
	if !piSelects(root, "extensions/tmag.ts", nil) || piSelects(root, "extensions/tmag.ts", &[]string{}) {
		t.Error("no filter selects everything, an empty one nothing")
	}
}

// Another program named pi or omp is no host of tmag's.
func TestOtherProgramsNamedLikeTheAgents(t *testing.T) {
	bin := t.TempDir()
	for name, line := range map[string]string{"pi": "pi 0.4 (digits of π)", "omp": "omp: version 2"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho '"+line+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	for _, h := range []host{piHostContext(context.Background()), ompHostContext(context.Background())} {
		if h.Present || h.Unavailable || setupPlan(h, true) != nil {
			t.Errorf("%s probed as %+v", h.CLI, h)
		}
	}
}
