package answer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOutput(t *testing.T) {
	question := Asked{Tool: "AskUserQuestion", Input: json.RawMessage(`{"questions":[{"question":"Pick a color"}]}`)}
	plan := Asked{Tool: "ExitPlanMode", Input: json.RawMessage(`{"plan":"do it"}`)}
	bash := Asked{Tool: "Bash", Input: json.RawMessage(`{"command":"date"}`)}
	for _, c := range []struct {
		name string
		a    Asked
		d    Decision
		want string // "" for an invalid decision
	}{
		{"allow", bash, Decision{Behavior: Allow}, `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`},
		{"deny with a message", bash, Decision{Behavior: Deny, Message: "not now"}, `{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"not now"},"hookEventName":"PermissionRequest"}}`},
		{"answer a question", question, Decision{Behavior: Allow, Answers: map[string]string{"Pick a color": "blue"}},
			`{"hookSpecificOutput":{"decision":{"behavior":"allow","updatedInput":{"answers":{"Pick a color":"blue"},"questions":[{"question":"Pick a color"}]}},"hookEventName":"PermissionRequest"}}`},
		{"approve a plan", plan, Decision{Behavior: Allow}, `{"hookSpecificOutput":{"decision":{"behavior":"allow","updatedInput":{"plan":"do it"}},"hookEventName":"PermissionRequest"}}`},
		{"a question without answers", question, Decision{Behavior: Allow}, ""},
		{"an answer to another question", question, Decision{Behavior: Allow, Answers: map[string]string{"Pick a colour": "blue"}}, ""},
		{"an unanswered question", Asked{Tool: "AskUserQuestion", Input: json.RawMessage(`{"questions":[{"question":"A"},{"question":"B"}]}`)},
			Decision{Behavior: Allow, Answers: map[string]string{"A": "x"}}, ""},
		{"answers for a tool", bash, Decision{Behavior: Allow, Answers: map[string]string{"q": "a"}}, ""},
		{"a message with allow", bash, Decision{Behavior: Allow, Message: "x"}, ""},
		{"no behavior", bash, Decision{}, ""},
	} {
		got, err := Output(c.a, c.d)
		switch {
		case c.want == "" && !errors.Is(err, ErrInvalid):
			t.Errorf("%s: %s, %v; want invalid", c.name, got, err)
		case c.want != "" && (err != nil || string(got) != c.want):
			t.Errorf("%s: %s, %v; want %s", c.name, got, err, c.want)
		}
	}
}

func TestWait(t *testing.T) {
	dir, err := os.MkdirTemp("", "ans") // socket paths are short
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	asked := Asked{Tool: "Bash", Input: json.RawMessage(`{"command":"date"}`)}

	wait := func(id string) (<-chan bool, *strings.Builder) {
		ln, err := Listen(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		done := make(chan bool, 1)
		go func() {
			defer ln.Close()
			done <- Wait(ctx, ln, asked, func(b []byte) error { out.Write(b); return nil })
		}()
		return done, &out
	}

	done, out := wait("r1")
	if got, err := Show(ctx, dir, "r1"); err != nil || got.Tool != "Bash" || string(got.Input) != `{"command":"date"}` {
		t.Fatalf("Show = %+v, %v", got, err)
	}
	if err := Decide(ctx, dir, "r1", Decision{Behavior: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid decision: %v", err)
	}
	if err := Decide(ctx, dir, "r1", Decision{Behavior: Deny, Message: "no"}); err != nil {
		t.Fatal(err)
	}
	if !<-done || !strings.Contains(out.String(), `"behavior":"deny"`) {
		t.Fatalf("hook printed %q", out.String())
	}

	done, out = wait("r2")
	Close(ctx, dir, "r2")
	emitted := <-done
	if ctx.Err() != nil {
		t.Fatalf("explicit close not observed before parent cancellation: %v", ctx.Err())
	}
	if emitted || out.Len() != 0 {
		t.Fatalf("a closed request printed %q", out.String())
	}
	path, err := socketPath(dir, "r2")
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if err := Decide(ctx, dir, "r2", Decision{Behavior: Allow}); !errors.Is(err, ErrGone) {
		t.Fatalf("deciding a request no hook waits on: %v", err)
	}
}

func TestWaitPrintsNothingOnceItEnds(t *testing.T) {
	dir, err := os.MkdirTemp("", "ans")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := Listen(dir, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan bool, 1)
	go func() { done <- Wait(ctx, ln, Asked{Tool: "Bash"}, func(b []byte) error { out.Write(b); return nil }) }()

	// A connection is being served when the hook is told to leave.
	path, err := socketPath(dir, "r1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(100 * time.Millisecond) // let Wait accept it
	cancel()
	_ = json.NewEncoder(conn).Encode(message{Op: "decide", Decision: &Decision{Behavior: Allow}})
	select {
	case emitted := <-done:
		if emitted || out.Len() != 0 {
			t.Fatalf("after its end the hook printed %q", out.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not end with its context")
	}
	var r reply
	if err := json.NewDecoder(conn).Decode(&r); err == nil && r.OK {
		t.Fatal("the decider was told the decision was printed")
	}
}
