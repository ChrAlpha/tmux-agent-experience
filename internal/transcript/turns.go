package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// Turn is one turn of a session as the agent's transcript tells it.
type Turn struct {
	// ID names the turn as the agent's hooks do.
	ID string `json:"id"`
	// Prompts are what the turn was given: the prompt that started it, if
	// it had one, then those that arrived while it ran.
	Prompts []Prompt `json:"prompts"`
	// Reply is what read --last gives once the turn ends: the text of its
	// final message. While the turn runs it is what the transcript holds so
	// far; Codex writes it when the turn ends.
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
// as far as they are written: the last one may be running.
func Turns(ctx context.Context, path, format string, n int) ([]Turn, error) {
	if n < 1 || n > MaxTurns {
		return nil, fmt.Errorf("read 1 to %d turns", MaxTurns)
	}
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
			turns = append(turns, bound(t))
		}
		return len(turns) == n
	})
	if err != nil {
		return nil, err
	}
	if len(turns) < n {
		if t, ok := r.end(); ok {
			turns = append(turns, bound(t))
		}
	}
	slices.Reverse(turns)
	return turns, nil
}

// A turnReader takes a transcript's lines, last first, and gives each turn
// once it has read the turn whole, with its prompts in the order they came.
type turnReader interface {
	line(b []byte) (Turn, bool)
	// end gives the turn being read at the transcript's start.
	end() (Turn, bool)
}

// claudeTurns reads Claude Code's JSONL transcript. A turn is the run of
// user entries that name the same prompt, as read --last and the hooks name
// it, with the assistant entries after each. Its prompts are its user entries
// with an origin, a peer's message (a meta entry), and the queued_command
// attachments of prompts that arrived while it ran. Tool results,
// interruption notes, compaction summaries, local commands and other meta
// entries are no prompts; a run the agent did not answer and no prompt
// started, such as a local command's, is no turn.
type claudeTurns struct {
	turn   Turn // the turn being read, its prompts last first; no ID yet before its last user entry
	active bool // the agent wrote in it
	// What was read since the last user entry: entries of the turn whose
	// user entry comes next.
	reply    claude
	prompts  []Prompt
	answered bool
}

func (c *claudeTurns) line(b []byte) (Turn, bool) {
	var e claudeEntry
	if json.Unmarshal(b, &e) != nil || e.IsSidechain {
		return Turn{}, false
	}
	switch {
	case e.Type == "attachment" && e.Attachment.Type == "queued_command":
		c.prompts = append(c.prompts, e.Attachment.Origin.prompt(words(e.Attachment.Prompt)))
		return Turn{}, false
	case e.Type != "user":
		c.answered = c.answered || e.Type == "assistant"
		c.reply.entry(e)
		return Turn{}, false
	case e.PromptID == "" || (e.IsMeta && e.Origin.Kind != "peer"):
		return Turn{}, false
	}
	// A user entry: what was read since belongs to its turn, and is that
	// turn's reply when the entry is the turn's last.
	var done Turn
	ended := false
	switch c.turn.ID {
	case e.PromptID:
		c.active = c.active || c.answered
	case "":
		c.turn, c.active = Turn{ID: e.PromptID, Reply: c.reply.words()}, c.answered
	default:
		done, ended = c.finish()
		c.turn, c.active = Turn{ID: e.PromptID, Reply: c.reply.words()}, c.answered
	}
	c.turn.Prompts = append(c.turn.Prompts, c.prompts...)
	if e.IsMeta || e.Origin.Kind != "" {
		c.turn.Prompts = append(c.turn.Prompts, e.Origin.prompt(words(e.Message.Content)))
	}
	c.reply, c.prompts, c.answered = claude{}, nil, false
	return done, ended
}

// end gives the turn being read when the transcript holds no earlier line.
func (c *claudeTurns) end() (Turn, bool) { return c.finish() }

// finish gives the turn read, unless it is no turn.
func (c *claudeTurns) finish() (Turn, bool) {
	t := c.turn
	if t.ID == "" || (len(t.Prompts) == 0 && !c.active) {
		return Turn{}, false
	}
	t.Prompts = reversed(t.Prompts)
	return t, true
}

// prompt is a prompt from this origin. A peer's message names its sender,
// and tmag names itself for the person.
func (o claudeOrigin) prompt(text string) Prompt {
	if o.Body != "" {
		text = o.Body
	}
	p := Prompt{Text: text}
	switch o.Kind {
	case "", "human":
	case "peer":
		if o.Name != "tmag" {
			p.From = "peer"
		}
	case "task-notification":
		p.From = "task"
		if summary := tagged(text, "summary"); summary != "" {
			p.Text = summary
		}
	default:
		p.From = o.Kind
	}
	return p
}

// tagged is the text of the first <tag> element in s, if any.
func tagged(s, tag string) string {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return ""
	}
	text, _, ok := strings.Cut(rest, "</"+tag+">")
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// codexTurns reads Codex's rollout: task_started starts a turn, each prompt
// is a UserMessage item of it, and task_complete carries its reply.
type codexTurns struct {
	turn Turn // the turn being read, its prompts last first
}

func (c *codexTurns) line(b []byte) (Turn, bool) {
	if !bytes.Contains(b, []byte(`"task_started"`)) && !bytes.Contains(b, []byte(`"task_complete"`)) &&
		!bytes.Contains(b, []byte(`"UserMessage"`)) {
		return Turn{}, false // most lines are long and of no interest: skip decoding
	}
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Type             string  `json:"type"`
			TurnID           string  `json:"turn_id"`
			LastAgentMessage *string `json:"last_agent_message"`
			Item             struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"item"`
		} `json:"payload"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "event_msg" || e.Payload.TurnID == "" {
		return Turn{}, false
	}
	id := e.Payload.TurnID
	if c.turn.ID != id {
		c.turn = Turn{ID: id} // a turn whose start the transcript did not hold is left out
	}
	switch e.Payload.Type {
	case "task_complete":
		if e.Payload.LastAgentMessage != nil {
			c.turn.Reply = *e.Payload.LastAgentMessage
		}
	case "item_completed":
		if e.Payload.Item.Type == "UserMessage" {
			c.turn.Prompts = append(c.turn.Prompts, Prompt{Text: words(e.Payload.Item.Content)})
		}
	case "task_started":
		t := c.turn
		t.Prompts = reversed(t.Prompts)
		c.turn = Turn{}
		return t, true
	}
	return Turn{}, false
}

// end leaves out a turn whose start the transcript does not hold.
func (c *codexTurns) end() (Turn, bool) { return Turn{}, false }

// piTurns reads Pi's and oh-my-pi's session file: a user message starts a
// turn, and its entry id names it.
type piTurns struct {
	reply pi
}

func (p *piTurns) line(b []byte) (Turn, bool) {
	var e piEntry
	if json.Unmarshal(b, &e) != nil || !p.reply.entry(e) {
		return Turn{}, false
	}
	t := Turn{ID: e.ID, Prompts: []Prompt{{Text: words(e.Message.Content)}}, Reply: p.reply.r.Text}
	p.reply = pi{}
	return t, true
}

// end leaves out the steps before the first user message.
func (p *piTurns) end() (Turn, bool) { return Turn{}, false }

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
