// Package transcript reads what an agent answered from the transcript it
// writes of a session, in the agent's own format.
//
// tmag stores no content: it reads a transcript only when a command asks for
// a reply, prints what it found and keeps nothing.
package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Formats, as eventlog.Transcript names them.
const (
	ClaudeJSONL  = "claude-jsonl"
	CodexRollout = "codex-rollout"
	PiJSONL      = "pi-jsonl"
)

// ErrNoTurn reports a transcript that holds no turn yet.
var ErrNoTurn = errors.New("no turn in the transcript")

// Reply is what the agent last answered: the text of its final message of
// a turn, "" when the turn ended without one.
type Reply struct {
	Turn string `json:"turn"`
	Text string `json:"text"`
}

// Last returns the reply of the last turn the transcript at path holds,
// as far as it is written.
func Last(ctx context.Context, path, format string) (Reply, error) {
	f, err := os.Open(path)
	if err != nil {
		return Reply{}, err
	}
	defer f.Close()
	var r replyReader
	switch format {
	case ClaudeJSONL:
		r = &claude{}
	case CodexRollout:
		r = &codex{}
	case PiJSONL:
		r = &pi{}
	default:
		return Reply{}, fmt.Errorf("unknown transcript format %q", format)
	}
	if err := backward(ctx, f, r.line); err != nil {
		return Reply{}, err
	}
	return r.reply()
}

// A replyReader takes a transcript's lines, last first, until it has the
// last turn's reply.
type replyReader interface {
	line(b []byte) (done bool)
	reply() (Reply, error)
}

// claude reads Claude Code's JSONL transcript. Each content block of an
// assistant message is a line of its own; every user entry, prompt or tool
// result, names the prompt whose turn it belongs to. A peer message is a
// meta user entry whose origin is a peer, and it names the turn it starts;
// other meta entries are skipped. The reply is the text of the last
// assistant message with any, after the turn's last user entry.
type claude struct {
	turn  string
	found bool
	have  bool     // the reply's message is found
	msg   string   // its id
	text  []string // its text blocks, last first
	ended bool     // an earlier message came after it
}

type claudeEntry struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	PromptID    string `json:"promptId"`
	Origin      struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func (c *claude) line(b []byte) bool {
	var e claudeEntry
	if json.Unmarshal(b, &e) != nil || e.IsSidechain {
		return false
	}
	switch e.Type {
	case "user":
		if e.PromptID == "" || (e.IsMeta && e.Origin.Kind != "peer") {
			return false
		}
		c.turn, c.found = e.PromptID, true
		return true
	case "assistant":
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return false
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
	return false
}

func (c *claude) reply() (Reply, error) {
	if !c.found {
		return Reply{}, ErrNoTurn
	}
	parts := make([]string, 0, len(c.text))
	for i := len(c.text) - 1; i >= 0; i-- {
		parts = append(parts, c.text[i])
	}
	return Reply{Turn: c.turn, Text: strings.Join(parts, "\n\n")}, nil
}

// codex reads Codex's rollout. A turn's end is a task_complete event that
// names the turn and carries the agent's last message.
type codex struct {
	r     Reply
	found bool
}

func (c *codex) line(b []byte) bool {
	if !bytes.Contains(b, []byte(`"task_complete"`)) {
		return false // most lines are long and of no interest: skip decoding
	}
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Type             string  `json:"type"`
			TurnID           string  `json:"turn_id"`
			LastAgentMessage *string `json:"last_agent_message"`
		} `json:"payload"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "event_msg" || e.Payload.Type != "task_complete" {
		return false
	}
	c.r.Turn, c.found = e.Payload.TurnID, true
	if e.Payload.LastAgentMessage != nil {
		c.r.Text = *e.Payload.LastAgentMessage
	}
	return true
}

func (c *codex) reply() (Reply, error) {
	if !c.found {
		return Reply{}, ErrNoTurn
	}
	return c.r, nil
}

// pi reads Pi's session file: a header, then one entry per line, each
// message whole. A turn starts with a user message, whose entry id names it;
// its reply is the text of the last assistant message with any after it.
// Other entries (tool results, system prompts, model changes, extensions'
// messages) belong to the turn they follow.
type pi struct {
	r     Reply
	found bool
	have  bool // the reply's message is found
}

func (p *pi) line(b []byte) bool {
	var e struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "message" {
		return false
	}
	switch e.Message.Role {
	case "user":
		p.r.Turn, p.found = e.ID, true
		return true
	case "assistant":
		if p.have {
			return false
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return false
		}
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		if len(texts) > 0 {
			p.r.Text, p.have = strings.Join(texts, "\n\n"), true
		}
	}
	return false
}

func (p *pi) reply() (Reply, error) {
	if !p.found {
		return Reply{}, ErrNoTurn
	}
	return p.r, nil
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
