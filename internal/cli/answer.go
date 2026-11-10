package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/answer"
	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
	"github.com/ChrAlpha/tmux-agent-experience/internal/hook"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func (a *app) cmdRequest(args []string) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 2 {
		err = errors.New("expected a target and a request id")
	}
	if err != nil {
		return a.usageError("request", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	asked, err := query.Show(ctx, a.log, pos[0], pos[1])
	if err != nil {
		return a.failOn("request", pos[0], err)
	}
	if a.json {
		a.emit(map[string]any{"session": asked.Session, "request": asked.Request, "tool": asked.Tool, "input": asked.Input})
		return ExitOK
	}
	fmt.Fprintf(a.stdout, "%s %s asks: %s\n", shortID(asked.Session.ID), asked.Request.Kind, asked.Tool)
	var pretty bytes.Buffer
	if json.Indent(&pretty, asked.Input, "", "  ") == nil {
		fmt.Fprintln(a.stdout, pretty.String())
	}
	return ExitOK
}

func (a *app) cmdAnswer(args []string) int {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	message := fs.String("message", "", "")
	answers := fs.String("answers", "", "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 3 {
		err = errors.New("expected a target, a request id, and allow or deny")
	}
	d := answer.Decision{Message: *message}
	if err == nil {
		d.Behavior = pos[2]
		if d.Behavior != answer.Allow && d.Behavior != answer.Deny {
			err = fmt.Errorf("decision %q is neither allow nor deny", d.Behavior)
		}
	}
	if err == nil && *answers != "" {
		if jerr := json.Unmarshal([]byte(*answers), &d.Answers); jerr != nil {
			err = fmt.Errorf("--answers takes a JSON object of question text to chosen label: %w", jerr)
		}
	}
	if err != nil {
		return a.usageError("answer", err)
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	dl, err := query.Answer(ctx, a.log, pos[0], pos[1], d)
	if err != nil {
		if errors.Is(err, answer.ErrInvalid) {
			return a.usageError("answer", err)
		}
		return a.failOn("answer", pos[0], err)
	}
	a.reportDelivery("answered", dl)
	return ExitOK
}

// answerWindow is how long an answer hook waits, a little less than the
// timeout its plugin gives it, so that it leaves on its own terms.
const answerWindow = 590 * time.Second

// cmdAnswerHook is Claude Code's PermissionRequest hook. It applies the event
// as `tmag hook` does, naming the prompt it opens, then waits for `tmag
// answer` while the agent's own dialog is on screen. Like `tmag hook` it
// always exits 0; it prints a decision delivered by `tmag answer` and nothing
// else, and on any failure, close, timeout or signal prints nothing, leaving
// the decision to the dialog.
func (a *app) cmdAnswerHook(args []string) (code int) {
	if len(args) != 1 || args[0] != proc.Claude {
		fmt.Fprintln(a.stderr, "usage: tmag answer-hook claude")
		return ExitOK
	}
	ctx, cancel := a.hookContext()
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			hook.RecordErrorContext(ctx, a.log.Dir, args[0], fmt.Errorf("answer hook panic: %v", r))
			code = ExitOK
		}
	}()
	if err := a.answerHook(); err != nil {
		hook.RecordErrorContext(ctx, a.log.Dir, args[0], err)
	}
	return ExitOK
}

func (a *app) answerHook() error {
	// A signal ends the wait from the start: whatever the hook has opened is
	// closed on the way out, and nothing is printed.
	ctx, stop := a.signalContext()
	defer stop()
	setup, finish := a.hookContext()
	defer finish()
	data, err := contextio.ReadAll(setup, a.stdin, 64<<20)
	if err != nil {
		return err
	}
	in := hook.Input{Agent: proc.Claude, Payload: bytes.NewReader(data), Log: a.log, PID: os.Getpid()}
	quick, cancel := context.WithTimeout(setup, time.Second)
	enabled := answer.Mode(quick) != answer.Off
	cancel()
	if !enabled {
		return hook.RunContext(setup, in)
	}
	var p struct {
		SessionID string          `json:"session_id"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return hook.RunContext(setup, in)
	}
	in.Request = session.NewRequestID()
	ln, err := answer.ListenContext(setup, a.log.Dir, in.Request)
	if err != nil {
		in.Request = ""
		return errors.Join(err, hook.RunContext(setup, in))
	}
	defer ln.Close()
	if err := hook.RunContext(setup, in); err != nil || ctx.Err() != nil {
		return err
	}
	// Wait only for a prompt the session now names this way.
	if rec, ok, err := (session.Store{Dir: a.log.Dir}).Get(setup, proc.Claude, p.SessionID); err != nil || !ok {
		return err
	} else if _, open := rec.Request(in.Request); !open {
		return nil
	}
	if err := setup.Err(); err != nil {
		return err
	}
	if a.setupDone != nil {
		a.setupDone()
	}
	ctx, cancel = context.WithTimeout(ctx, answerWindow-250*time.Millisecond)
	defer cancel()
	writer := a.stdout
	if f, ok := writer.(*os.File); ok {
		owned, err := contextio.Own(ctx, f)
		if err != nil {
			return err
		}
		defer owned.Close()
		writer = owned
	}
	answer.Wait(ctx, ln, answer.Asked{Tool: p.ToolName, Input: p.ToolInput}, func(out []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := writer.Write(out)
		return err
	})
	return nil
}
