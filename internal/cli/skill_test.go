package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// skills are the copies of the tmag-peers skill, one per host, which must
// not differ: the text says how each host runs tmag.
var skills = []string{
	"plugins/claude/tmag-peers/skills/peers/SKILL.md",
	"plugins/codex/tmag-peers/skills/peers/SKILL.md",
}

func readSkill(t *testing.T) string {
	t.Helper()
	var text string
	for i, path := range skills {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && string(b) != text {
			t.Fatalf("%s differs from %s", path, skills[0])
		}
		text = string(b)
	}
	return text
}

// The skill teaches the CLI as it is: its commands, flags, refusal codes
// and exit codes.
func TestPeersSkillMatchesCLI(t *testing.T) {
	text := readSkill(t)
	flat := strings.Join(strings.Fields(text), " ")

	m := regexp.MustCompile(`(?s)\A---\nname: (\S+)\ndescription: ([^\n]+)\n---\n`).FindStringSubmatch(text)
	if m == nil || m[1] != "peers" {
		t.Fatal("frontmatter must be name: peers, then a one-line description")
	}
	if n := len(m[2]); n > 1024 {
		t.Errorf("description is %d characters; hosts truncate past 1024", n)
	}
	// A plain YAML scalar ends at ": " or " #", and cannot start with an
	// indicator; a strict parser would reject or cut the description.
	if d := m[2]; strings.Contains(d, ": ") || strings.Contains(d, " #") || strings.ContainsAny(d[:1], "-?:,[]{}#&*!|>'\"%@`") {
		t.Errorf("description is not a plain YAML scalar: %q", d)
	}

	commands := map[string]bool{}
	for _, c := range regexp.MustCompile(`(?m)^  tmag (\S+)`).FindAllStringSubmatch(usage, -1) {
		commands[c[1]] = true
	}
	for _, c := range regexp.MustCompile("`tmag ([a-z-]+)").FindAllStringSubmatch(text, -1) {
		if !commands[c[1]] {
			t.Errorf("the skill names tmag %s, which the CLI does not have", c[1])
		}
	}
	for _, f := range regexp.MustCompile(` (--[a-z][a-z-]*)`).FindAllStringSubmatch(text, -1) {
		if !strings.Contains(usage, f[1]) {
			t.Errorf("the skill names %s, which no command takes", f[1])
		}
	}

	for _, phrase := range []string{
		fmt.Sprintf("exits %d with `refused (CODE): reason`", ExitRefused),
		fmt.Sprintf("Exit %d from `send --wait` or `wait` means the wait timed out", ExitTimeout),
		fmt.Sprintf("exit %d then means nothing was delivered", ExitTimeout),
		fmt.Sprintf("Exit %d is an error, such as `ended`", ExitError),
		"`no_reply` (its last turn left no reply",
		fmt.Sprintf("starting with `%s`, says what to do", strings.TrimSpace(nextPrefix)),
		fmt.Sprintf("starting with `%s` is refused with exit %d", query.Marker, ExitUsage),
		fmt.Sprintf("exit %d is a usage error", ExitUsage),
		"Exit 127 with `tmag: not installed`",
	} {
		if !strings.Contains(flat, phrase) {
			t.Errorf("the skill no longer says %q", phrase)
		}
	}
}

// The skill leaves what to do after each refusal to the line the CLI prints
// after it, so every refusal reason must have one.
func TestEveryRefusalSaysWhatNext(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "query", "control.go"))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, c := range regexp.MustCompile(`(?m)^\s+Refuse\w+\s+= "(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		codes = append(codes, c[1])
	}
	if len(codes) == 0 {
		t.Fatal("found no refusal reasons in query/control.go")
	}
	for _, code := range codes {
		for _, cmd := range []string{"send", "reply", "interrupt", "answer", "read"} {
			if next(cmd, "abcd", &query.RefusedError{Reason: code}) == "" {
				t.Errorf("%s refused %s says nothing about what to do next", cmd, code)
			}
		}
	}
	for _, err := range []error{query.ErrEnded, query.ErrNoReply, query.ErrNotFound, query.ErrAmbiguous} {
		if next("send", "abcd", err) == "" {
			t.Errorf("%v says nothing about what to do next", err)
		}
	}
	for _, cmd := range []string{"send", "wait", "reply"} {
		if next(cmd, "abcd", errTimeout) == "" {
			t.Errorf("a timed out %s says nothing about what to do next", cmd)
		}
	}
}

// What to do next follows the error on stderr, naming the target as typed,
// and stays out of the JSON document.
func TestNextFollowsTheError(t *testing.T) {
	var stdout, stderr strings.Builder
	a := &app{stdout: &stdout, stderr: &stderr, json: true}
	if code := a.failOn("send", "01a1", &query.RefusedError{Reason: query.RefuseBusy, Detail: "01a19fda is working"}); code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
	want := "tmag send: refused (busy): 01a19fda is working\n" + nextPrefix + "wait until it is idle with tmag wait 01a1 --for idle, then try once more\n"
	if stderr.String() != want {
		t.Errorf("stderr is %q, want %q", stderr.String(), want)
	}
	if strings.Contains(stdout.String(), "tmag wait") {
		t.Errorf("the JSON document carries the advice: %s", stdout.String())
	}
}
