package transcript

import "encoding/json"

// piEntry is a line of Pi's and oh-my-pi's session file: a header, then one
// entry per line, each message whole.
type piEntry struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// piTurns reads Pi's and oh-my-pi's session file. A user message starts a
// turn, and its entry id names it; the turn's reply is the text of the last
// assistant message with any after it. Other entries (tool results, system
// prompts, model changes, extensions' messages) belong to the turn they
// follow, and the steps before the first user message to none.
type piTurns struct {
	reply string // what was read since the last user message: its turn's reply
	have  bool   // the reply's message is found
}

func (p *piTurns) line(b []byte) (Turn, bool) {
	var e piEntry
	if json.Unmarshal(b, &e) != nil || e.Type != "message" {
		return Turn{}, false
	}
	switch e.Message.Role {
	case "user":
		t := Turn{ID: e.ID, Prompts: []Prompt{{Text: words(e.Message.Content)}}, Reply: p.reply}
		*p = piTurns{}
		return t, true
	case "assistant":
		if text := words(e.Message.Content); !p.have && text != "" {
			p.reply, p.have = text, true
		}
	}
	return Turn{}, false
}

// end leaves out the steps before the first user message.
func (p *piTurns) end() (Turn, bool) { return Turn{}, false }
