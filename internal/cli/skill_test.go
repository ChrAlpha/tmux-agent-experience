package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// skills are the copies of the tmag-peers skill, one per host, which must
// not differ: the text says how each host runs tmag.
var skills = []string{
	"plugins/claude/tmag-peers/skills/peers/SKILL.md",
	"plugins/codex/tmag-peers/skills/peers/SKILL.md",
	"plugins/pi/tmag-peers/skills/peers/SKILL.md",
	"plugins/omp/tmag-peers/skills/peers/SKILL.md",
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

// usageCommands are the commands the CLI's usage lists.
func usageCommands() map[string]bool {
	commands := map[string]bool{}
	for _, c := range regexp.MustCompile(`(?m)^  tmag (\S+)`).FindAllStringSubmatch(usage, -1) {
		commands[c[1]] = true
	}
	return commands
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

	commands := usageCommands()
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
		fmt.Sprintf("A timeout exits %d, and its error says whether the prompt was delivered", ExitTimeout),
		"run it again only when it says nothing was delivered",
		fmt.Sprintf("Exit %d is an error, such as `ended`", ExitError),
		"`no_reply` (its last turn left no reply",
		fmt.Sprintf("the line after the error says it, starting with `%s`", strings.TrimSpace(nextPrefix)),
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
}

// What to do after a failed delivery depends on whether the prompt went
// out: only a proven delivery forbids sending it again, and only a proven
// non-delivery invites running the command again. Anything else leaves the
// error's own words, which say what is known.
func TestNextKnowsWhetherAPromptWasDelivered(t *testing.T) {
	after := delivered{fmt.Errorf("prompt delivered at seq 9; subsequent outcome unknown, do not repeat delivery: %w", errTimeout)}
	for _, c := range []struct {
		cmd  string
		err  error
		want string // "" for no advice
	}{
		{"send", after, "run tmag wait %3 --for idle,blocked, then tmag read %3 --last; do not send the prompt again"},
		{"send", delivered{fmt.Errorf("x: %w", query.ErrEnded)}, "the prompt arrived, but its session ended"},
		{"send", errTimeout, ""},
		{"send", context.DeadlineExceeded, ""},
		{"reply", undelivered{errTimeout}, "nothing was delivered: run the same tmag reply again"},
		{"reply", errTimeout, ""},
		{"wait", errTimeout, "run the same tmag wait again"},
		{"interrupt", &query.RefusedError{Reason: query.RefuseBusy}, "tmag wait %3 --for idle shows when it stops"},
		{"send", &query.NotTakenError{Seq: 9, Target: "%3", Err: errors.New("the extension took nothing")}, "nothing was delivered: run the same tmag send again"},
		{"send", &query.RefusedError{Reason: query.RefuseBusy}, "try once more in a few seconds"},
		{"send", &query.RefusedError{Reason: query.RefuseUnbound}, "nothing tmag can prove reaches it"},
		{"send", &query.RefusedError{Reason: query.RefuseUnsupported}, "only the person can do this"},
	} {
		got := next(c.cmd, "%3", c.err)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s after %v: next is %q, want %q", c.cmd, c.err, got, c.want)
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
	want := "tmag send: refused (busy): 01a19fda is working\n" + nextPrefix + "try once more in a few seconds, or wait until it is idle with tmag wait 01a1 --for idle\n"
	if stderr.String() != want {
		t.Errorf("stderr is %q, want %q", stderr.String(), want)
	}
	if strings.Contains(stdout.String(), "tmag wait") {
		t.Errorf("the JSON document carries the advice: %s", stdout.String())
	}
}

// A reply that times out says nothing was delivered only when its last try
// was refused, which delivers nothing; after any other error, such as an
// agent service that took the request and did not answer, the outcome is
// unknown and nothing invites running it again. Either way it exits 4.
func TestReplyTimeoutClaimsNothingUnproven(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	a := &app{stdout: io.Discard, stderr: io.Discard}
	refused := a.deadline(ctx, time.Second, &query.RefusedError{Reason: query.RefuseBusy, Detail: "01a19fda is working"})
	if !strings.Contains(refused.Error(), "nothing delivered") || !strings.Contains(next("reply", "01a1", refused), "run the same tmag reply again") {
		t.Errorf("after a refusal: %q, next %q", refused, next("reply", "01a1", refused))
	}
	uncertain := a.deadline(ctx, time.Second, errors.New("logged at seq 9, but the Codex daemon did not take it: i/o timeout"))
	if strings.Contains(uncertain.Error(), "nothing delivered") || next("reply", "01a1", uncertain) != "" {
		t.Errorf("after an uncertain delivery: %q, next %q", uncertain, next("reply", "01a1", uncertain))
	}
	for _, err := range []error{refused, uncertain} {
		if code := a.failOn("reply", "01a1", err); code != ExitTimeout {
			t.Errorf("%v exits %d, want %d", err, code, ExitTimeout)
		}
	}
}
