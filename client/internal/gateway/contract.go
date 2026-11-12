package gateway

import "time"

// Session is a session as tmag/v2 reports it in a snapshot or a state event.
// A state event reports the whole session; a field left out is empty.
type Session struct {
	LastHookAt   time.Time   `json:"last_hook_at,omitzero"`
	ID           string      `json:"id"`
	Agent        string      `json:"agent"`
	State        string      `json:"state"`
	Kind         string      `json:"kind,omitempty"`
	Turn         string      `json:"turn,omitempty"`
	Seen         string      `json:"seen,omitempty"`
	Since        *time.Time  `json:"since,omitempty"`
	Cwd          string      `json:"cwd,omitempty"`
	Runtime      string      `json:"runtime,omitempty"`
	Transcript   *Transcript `json:"transcript,omitempty"`
	Subagents    []string    `json:"subagents,omitempty"`
	Capabilities []string    `json:"capabilities"`
	Requests     []Request   `json:"requests,omitempty"`
}

// Transcript is where an agent writes a session's transcript.
type Transcript struct {
	Path   string `json:"path"`
	Format string `json:"format"`
}

// Request is an open prompt the session waits on the person for.
type Request struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool,omitempty"`
	Agent string    `json:"agent,omitempty"`
	Since time.Time `json:"since"`
}

// Pane is an agent client in a tmux pane and the session it shows.
type Pane struct {
	Server   string `json:"server"`
	ID       string `json:"id"`
	Target   string `json:"target"`
	Cwd      string `json:"cwd"`
	Command  string `json:"command"`
	Agent    string `json:"agent"`
	Session  string `json:"session,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	View     string `json:"view,omitempty"`
}

// Alert is the change an Alert event reports, without any session state.
type Alert struct {
	Kind    string        `json:"kind"`
	Turn    string        `json:"turn,omitempty"`
	Present bool          `json:"present"`
	Request *AlertRequest `json:"request,omitempty"`
}

// AlertRequest is the request a blocked alert opened.
type AlertRequest struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Tool string `json:"tool,omitempty"`
}

// Event is one tmag/v2 event, including the fields a client reads.
type Event struct {
	LastHookAt   time.Time   `json:"last_hook_at,omitzero"`
	Seq          int64       `json:"seq"`
	Time         time.Time   `json:"time"`
	Agent        string      `json:"agent"`
	Session      string      `json:"session"`
	Event        string      `json:"event"`
	State        string      `json:"state,omitempty"`
	Kind         string      `json:"kind,omitempty"`
	Since        *time.Time  `json:"since,omitempty"`
	Turn         string      `json:"turn,omitempty"`
	Seen         string      `json:"seen,omitempty"`
	Cwd          string      `json:"cwd,omitempty"`
	Runtime      string      `json:"runtime,omitempty"`
	Transcript   *Transcript `json:"transcript,omitempty"`
	Subagents    []string    `json:"subagents,omitempty"`
	Capabilities []string    `json:"capabilities,omitempty"`
	Requests     []Request   `json:"requests,omitempty"`
	Alert        *Alert      `json:"alert,omitempty"`
}

// ended reports whether the event ends its session.
func (e Event) ended() bool { return e.Event == "SessionEnd" || e.Event == "Exit" }
