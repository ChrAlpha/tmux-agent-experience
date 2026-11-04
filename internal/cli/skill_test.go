package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
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

	src, err := os.ReadFile(filepath.Join("..", "query", "control.go"))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, c := range regexp.MustCompile(`(?m)^\s+Refuse\w+\s+= "(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		codes = append(codes, c[1])
	}
	_, section, _ := strings.Cut(text, "\n## Refusals\n")
	section, _, _ = strings.Cut(section, "\n## ")
	var listed []string
	for _, c := range regexp.MustCompile("(?m)^\\| `(\\w+)` \\|").FindAllStringSubmatch(section, -1) {
		listed = append(listed, c[1])
	}
	slices.Sort(codes)
	slices.Sort(listed)
	if len(codes) == 0 || !slices.Equal(codes, listed) {
		t.Errorf("refusal table lists %v, the CLI refuses with %v", listed, codes)
	}

	for _, phrase := range []string{
		fmt.Sprintf("exits %d with `refused (CODE): reason`", ExitRefused),
		fmt.Sprintf("Exit %d means the wait timed out", ExitTimeout),
		fmt.Sprintf("Exit %d is an error (`ended`", ExitError),
		fmt.Sprintf("exit %d is a usage error", ExitUsage),
		"Exit 127 with `tmag: not installed`",
	} {
		if !strings.Contains(flat, phrase) {
			t.Errorf("the skill no longer says %q", phrase)
		}
	}
}
