package version

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// node returns the Node.js that runs TypeScript as it is (22.6 or later,
// which strips types), or skips the test.
func node(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(path, "-p", "process.versions.node").Output()
	var major, minor int
	if err != nil {
		t.Skipf("node: %v", err)
	}
	if _, err := fmt.Sscanf(string(out), "%d.%d", &major, &minor); err != nil || major < 22 || major == 22 && minor < 6 {
		t.Skipf("node %s strips no types", strings.TrimSpace(string(out)))
	}
	return path
}

// The extensions report what TestPiAndOhMyPi's fake agents report, from the
// events Pi and oh-my-pi send them: they run, in order and once each, the
// hooks of the state changes, naming the turn by the entry id the agent
// records for its prompt, and nothing of the prompt, the reply or a tool's
// input. Pi records a user message after the extensions have it,
// oh-my-pi before; oh-my-pi runs the extension for subagents too, and moves
// its process to other sessions with or without a word.
func TestExtensionsReport(t *testing.T) {
	node := node(t)
	for host, want := range map[string][]string{
		"pi": {
			"session_start s1 source=resume turn=e0",
			"message_end s1 turn=e2",
			"tool_execution_start s1 tool=bash call=c1",
			"ui_prompt_start s1",
			"ui_prompt_end s1",
			"tool_execution_end s1 tool=bash call=c1",
			"agent_settled s1 stop=stop",
			"agent_start s1",
			"agent_settled s1 stop=aborted",
			"agent_start s1",
			"agent_settled s1 stop=error",
			"session_shutdown s1 reason=quit",
		},
		"omp": {
			"session_start s1 source=startup turn=e0",
			"message_end s1 turn=e2",
			"tool_execution_start s1 tool=ask call=c1",
			"tool_execution_end s1 tool=ask call=c1",
			"tool_execution_start s1 tool=bash call=c2",
			"tool_approval_requested s1 tool=bash call=c2",
			"tool_approval_resolved s1 tool=bash call=c2",
			"tool_execution_end s1 tool=bash call=c2",
			"agent_end s1 stop=stop",
			"session_shutdown s1 reason=branch",
			"session_switch s2 source=branch turn=e2",
			"session_shutdown s2 reason=switch",
			"session_switch s3 source=switch turn=e2",
			"agent_start s3",
			"agent_end s3 stop=stop",
			"session_shutdown s3 reason=quit",
		},
	} {
		t.Run(host, func(t *testing.T) {
			tpm, log := t.TempDir(), filepath.Join(t.TempDir(), "hooks")
			tmag := filepath.Join(tpm, "tmux-agent-experience", "bin", "tmag")
			if err := os.MkdirAll(filepath.Dir(tmag), 0o755); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n{ printf '%s %s ' \"$1\" \"$2\"; cat; echo; } >>\"$TMAG_LOG\"\n"
			if err := os.WriteFile(tmag, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			extension, err := filepath.Abs(filepath.Join("..", "..", "plugins", host, "tmag", "extensions", "tmag.ts"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", filepath.Join("testdata", "extension.mjs"), host, extension)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TMUX_PLUGIN_MANAGER_PATH=" + tpm, "TMAG_LOG=" + log}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "secret") {
				t.Errorf("a report carries content:\n%s", data)
			}
			var got []string
			sc := bufio.NewScanner(strings.NewReader(string(data)))
			for sc.Scan() {
				args, body, _ := strings.Cut(sc.Text(), " {")
				var p map[string]any
				if args != "hook "+host || json.Unmarshal([]byte("{"+body), &p) != nil {
					t.Fatalf("ran tmag %s with %q", args, body)
				}
				if p["cwd"] != "/work" || p["mode"] != "tui" || p["transcript_path"] != nil && p["transcript_path"] != "/sessions/"+p["session_id"].(string)+".jsonl" {
					t.Errorf("payload %v", p)
				}
				line := fmt.Sprintf("%s %s", p["hook_event_name"], p["session_id"])
				for _, f := range [][2]string{{"source", "source"}, {"reason", "reason"}, {"turn_id", "turn"}, {"tool_name", "tool"}, {"tool_use_id", "call"}, {"stop_reason", "stop"}} {
					if v, ok := p[f[0]]; ok {
						line += fmt.Sprintf(" %s=%v", f[1], v)
					}
				}
				got = append(got, line)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("reported\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}
