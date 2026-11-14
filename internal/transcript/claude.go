package transcript

import (
	"encoding/json"
	"strings"
)

// claudeEntry is a line of Claude Code's JSONL transcript. Each content block
// of an assistant message is a line of its own; every user entry, prompt or
// tool result, names the prompt whose turn it belongs to.
type claudeEntry struct {
	Type        string       `json:"type"`
	IsSidechain bool         `json:"isSidechain"`
	IsMeta      bool         `json:"isMeta"`
	PromptID    string       `json:"promptId"`
	Origin      claudeOrigin `json:"origin"`
	Attachment  struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
		Origin claudeOrigin    `json:"origin"`
	} `json:"attachment"`
	Message struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeOrigin is where a prompt came from: the person (human), a
// background task's notice (task-notification), or a message another
// session sent (peer), whose sender it names and whose text it carries.
type claudeOrigin struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Body string `json:"body"`
}

// claudeTurns reads Claude Code's JSONL transcript. A turn is the run of
// user entries that name the same prompt, as the hooks name it, with the
// assistant entries after each; its reply is the text of the last assistant
// message with any after its last user entry. Its prompts are its user
// entries with an origin, a peer's message (a meta entry), and the
// queued_command attachments of prompts that arrived while it ran. Tool
// results, interruption notes, compaction summaries, local commands and
// other meta entries are no prompts; a run the agent did not answer and no
// prompt started, such as a local command's, is no turn.
type claudeTurns struct {
	turn   Turn // the turn being read, its prompts last first; no ID yet before its last user entry
	active bool // the agent wrote in it
	// What was read since the last user entry: entries of the turn whose
	// user entry comes next.
	reply    claudeReply
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
	case e.Type == "assistant":
		c.answered = true
		c.reply.assistant(e)
		return Turn{}, false
	case e.Type != "user" || e.PromptID == "" || (e.IsMeta && e.Origin.Kind != "peer"):
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
	c.reply, c.prompts, c.answered = claudeReply{}, nil, false
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

// claudeReply takes assistant entries, last first, and keeps the text of the
// last message with any, whose blocks share its id.
type claudeReply struct {
	have  bool     // the reply's message is found
	msg   string   // its id
	text  []string // its text blocks, last first
	ended bool     // an earlier message came after it
}

func (c *claudeReply) assistant(e claudeEntry) {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return
	}
	var texts []string
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == "text" && blocks[i].Text != "" {
			texts = append(texts, blocks[i].Text)
		}
	}
	switch {
	case c.ended:
	case !c.have:
		if len(texts) > 0 {
			c.have, c.msg, c.text = true, e.Message.ID, texts
		}
	case e.Message.ID == c.msg && c.msg != "":
		c.text = append(c.text, texts...)
	default:
		c.ended = true
	}
}

// words is the reply's text, its blocks in the order they were written.
func (c *claudeReply) words() string {
	parts := make([]string, 0, len(c.text))
	for i := len(c.text) - 1; i >= 0; i-- {
		parts = append(parts, c.text[i])
	}
	return strings.Join(parts, "\n\n")
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
