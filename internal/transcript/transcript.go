// Package transcript reads a session's turns, what the agent was asked and
// what it answered, from the transcript it writes, in the agent's own format.
//
// tmag stores no content: it reads a transcript only when a command asks for
// turns or a reply, prints what it found and keeps nothing.
package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// Formats, as eventlog.Transcript names them.
const (
	ClaudeJSONL  = "claude-jsonl"
	CodexRollout = "codex-rollout"
	PiJSONL      = "pi-jsonl"
)

// ErrNoTurn reports a transcript that holds no turn yet.
var ErrNoTurn = errors.New("no turn in the transcript")

// Turn is one turn of a session as the agent's transcript tells it.
type Turn struct {
	// ID names the turn as the agent's hooks do.
	ID string `json:"id"`
	// Prompts are what the turn was given: the prompt that started it, if
	// it had one, then those that arrived while it ran.
	Prompts []Prompt `json:"prompts"`
	// Reply is the text of the turn's final message once the turn ends, ""
	// when it ended without one. While the turn runs it is what the
	// transcript holds so far; Codex writes it when the turn ends.
	Reply string `json:"reply"`
}

// Prompt is a prompt a turn was given. From is who sent it when the
// transcript names someone other than the person: peer for another
// session's message, task for a background task's notice. A prompt an agent
// sent through tmag starts with the line tmag wrote for it.
type Prompt struct {
	Text string `json:"text"`
	From string `json:"from,omitempty"`
}

// Bounds of what Turns returns, whatever the transcript holds.
const (
	MaxTurns   = 20
	maxPrompts = 10       // per turn: the first and the last ones
	maxText    = 16 << 10 // bytes of a prompt or a reply
)

// Turns returns the last n turns the transcript at path holds, oldest first,
// as far as they are written: the last one may be running. Each keeps within
// the bounds above.
func Turns(ctx context.Context, path, format string, n int) ([]Turn, error) {
	if n < 1 || n > MaxTurns {
		return nil, fmt.Errorf("read 1 to %d turns", MaxTurns)
	}
	turns, err := read(ctx, path, format, n)
	for i := range turns {
		turns[i] = bound(turns[i])
	}
	return turns, err
}

// Last returns the last turn the transcript at path holds, as far as it is
// written, its reply whole: what read --last prints once the turn ends.
func Last(ctx context.Context, path, format string) (Turn, error) {
	turns, err := read(ctx, path, format, 1)
	switch {
	case err != nil:
		return Turn{}, err
	case len(turns) == 0:
		return Turn{}, ErrNoTurn
	}
	return turns[0], nil
}

// A turnReader takes a transcript's lines, last first, and gives each turn
// once it has read the turn whole, with its prompts in the order they came.
type turnReader interface {
	line(b []byte) (Turn, bool)
	// end gives the turn being read at the transcript's start.
	end() (Turn, bool)
}

// read returns the last n turns the transcript at path holds, oldest first.
func read(ctx context.Context, path, format string, n int) ([]Turn, error) {
	var r turnReader
	switch format {
	case ClaudeJSONL:
		r = &claudeTurns{}
	case CodexRollout:
		r = &codexTurns{}
	case PiJSONL:
		r = &piTurns{}
	default:
		return nil, fmt.Errorf("unknown transcript format %q", format)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var turns []Turn
	err = backward(ctx, f, func(b []byte) bool {
		if t, ok := r.line(b); ok {
			turns = append(turns, t)
		}
		return len(turns) == n
	})
	if err != nil {
		return nil, err
	}
	if len(turns) < n {
		if t, ok := r.end(); ok {
			turns = append(turns, t)
		}
	}
	slices.Reverse(turns)
	return turns, nil
}

// words is the text of a message's content: a string, or blocks of which
// the text ones count.
func words(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// reversed returns the prompts read last first in the order they came.
func reversed(p []Prompt) []Prompt {
	p = slices.Clone(p)
	slices.Reverse(p)
	return p
}

// bound keeps a turn within the bounds Turns promises: its first prompt and
// last ones, each text cut.
func bound(t Turn) Turn {
	if t.Prompts == nil {
		t.Prompts = []Prompt{}
	}
	if len(t.Prompts) > maxPrompts {
		t.Prompts = append(t.Prompts[:1:1], t.Prompts[len(t.Prompts)-maxPrompts+1:]...)
	}
	for i := range t.Prompts {
		t.Prompts[i].Text = cut(t.Prompts[i].Text)
	}
	t.Reply = cut(t.Reply)
	return t
}

// cut ends s within maxText bytes, at a character's boundary.
func cut(s string) string {
	if len(s) <= maxText {
		return s
	}
	const more = "…"
	end := maxText - len(more)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + more
}

// chunk is how much of the file backward reads at a time.
const chunk = 64 << 10

// backward calls fn with each line of f, last first, until fn returns true.
// It reads the file as it was when it started; a line being written then
// comes incomplete, so it does not decode.
func backward(ctx context.Context, f *os.File, fn func([]byte) bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	var tail []byte // the start of the line the last chunk ended in
	for end > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(chunk), end)
		buf := make([]byte, n, int(n)+len(tail))
		if _, err := f.ReadAt(buf, end-n); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		end -= n
		buf = append(buf, tail...)
		for {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				break
			}
			if line := bytes.TrimSpace(buf[i+1:]); len(line) > 0 && fn(line) {
				return nil
			}
			buf = buf[:i]
		}
		tail = buf
	}
	if line := bytes.TrimSpace(tail); len(line) > 0 {
		fn(line)
	}
	return nil
}
