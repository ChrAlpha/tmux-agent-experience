package transcript

import (
	"bytes"
	"encoding/json"
)

// codexTurns reads Codex's rollout: task_started starts a turn, each prompt
// is a UserMessage item of it, and task_complete ends it with the agent's
// last message.
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
