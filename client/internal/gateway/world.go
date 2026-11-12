package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
)

// Machine statuses: connecting until its first document, online while its
// watch runs, offline once it broke, until a new watch sends a document.
const (
	Connecting = "connecting"
	Online     = "online"
	Offline    = "offline"
)

// MachineView is what the page knows of a machine.
type MachineView struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
	Since  time.Time `json:"since"`
}

// View is a session as the page shows it: tmag's session, the panes that
// show it, and whether its last turn waits to be read.
type View struct {
	Key     string `json:"key"`
	Machine string `json:"machine"`
	machine.Session
	Panes []PaneView `json:"panes,omitempty"`
	// Unread is an idle session whose last turn nobody opened.
	Unread bool `json:"unread"`
}

// PaneView is a pane that shows a session.
type PaneView struct {
	Server string `json:"server"`
	ID     string `json:"id"`
	Target string `json:"target"`
	View   string `json:"view,omitempty"`
}

// key names a session across machines.
func key(machineName, agent, id string) string { return machineName + "/" + agent + "/" + id }

// machineOf is the machine a key names.
func machineOf(k string) (string, bool) {
	parts := strings.SplitN(k, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[0], true
}

// tracked is one machine's sessions and panes as its watch reports them,
// and the views last sent for them.
type tracked struct {
	view     MachineView
	sessions map[string]machine.Session // by key
	panes    []machine.Pane
	sent     map[string][]byte // key → the view's JSON pages have
	cursor   int64
}

// world is every machine's sessions, the turns people have read, and the
// pages that follow them. One lock guards it; nothing under it waits on a
// machine or a page.
type world struct {
	mu       sync.Mutex
	order    []string
	machines map[string]*tracked
	seen     *seenTurns
	streams  map[*stream]struct{}
	now      func() time.Time
}

func newWorld(names []string, seen *seenTurns) *world {
	w := &world{order: names, machines: map[string]*tracked{}, seen: seen, streams: map[*stream]struct{}{}, now: time.Now}
	for _, n := range names {
		w.machines[n] = &tracked{view: MachineView{Name: n, Status: Connecting, Since: w.now()}, sessions: map[string]machine.Session{}, sent: map[string][]byte{}, cursor: -1}
	}
	return w
}

// cursor is where a machine's next watch resumes.
func (w *world) cursor(name string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.machines[name].cursor
}

// apply takes one watch document of a machine. It returns the event when
// the document was an Alert, for the caller to judge with alert.
func (w *world) apply(name string, d machine.Doc) *machine.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.machines[name]
	if t.view.Status != Online {
		t.view = MachineView{Name: name, Status: Online, Since: w.now()}
		w.broadcast("machine", t.view)
	}
	var alert *machine.Event
	switch d.Type {
	case "snapshot":
		t.sessions = map[string]machine.Session{}
		for _, s := range d.Sessions {
			t.sessions[key(name, s.Agent, s.ID)] = s
		}
		t.panes = d.Panes
		t.cursor = d.Seq
	case "panes":
		t.panes = d.Panes
	case "event":
		e := d.Event
		if e == nil {
			return nil
		}
		t.cursor = e.Seq
		k := key(name, e.Agent, e.Session)
		switch {
		case e.Ended():
			delete(t.sessions, k)
		case e.State != "":
			t.sessions[k] = e.Reported()
		case e.Event == "Alert" && e.Alert != nil:
			alert = e
		}
	}
	w.publish(name)
	return alert
}

// fail marks a machine offline with why. Its sessions stay as last known.
func (w *world) fail(name string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.machines[name]
	msg := err.Error()
	if t.view.Status == Offline && t.view.Error == msg {
		return
	}
	since := w.now()
	if t.view.Status == Offline {
		since = t.view.Since
	}
	t.view = MachineView{Name: name, Status: Offline, Error: msg, Since: since}
	w.broadcast("machine", t.view)
}

// views returns a machine's views, by key.
func (w *world) views(name string) map[string]View {
	t := w.machines[name]
	panes := map[string][]PaneView{}
	for _, p := range t.panes {
		if p.Session != "" {
			k := key(name, p.Agent, p.Session)
			panes[k] = append(panes[k], PaneView{Server: p.Server, ID: p.ID, Target: p.Target, View: p.View})
		}
	}
	out := make(map[string]View, len(t.sessions))
	for k, s := range t.sessions {
		out[k] = View{Key: k, Machine: name, Session: s, Panes: panes[k],
			Unread: s.State == "idle" && s.Turn != "" && w.seen.turn(k) != s.Turn}
	}
	return out
}

// publish sends pages every view of the machine that changed since they got
// it, and the end of every session that went.
func (w *world) publish(name string) {
	t := w.machines[name]
	now := w.views(name)
	for k, v := range now {
		b, _ := json.Marshal(v)
		if !bytes.Equal(t.sent[k], b) {
			t.sent[k] = b
			w.broadcastRaw("session", b)
		}
	}
	for k := range t.sent {
		if _, ok := now[k]; !ok {
			delete(t.sent, k)
			w.broadcast("gone", map[string]string{"key": k})
		}
	}
}

// session returns the session a key names, as last reported.
func (w *world) session(k string) (View, MachineView, bool) {
	name, ok := machineOf(k)
	if !ok {
		return View{}, MachineView{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.machines[name]
	if !ok {
		return View{}, MachineView{}, false
	}
	v, ok := w.views(name)[k]
	return v, t.view, ok
}

// errStale is a mark for a turn the session is no longer idle on.
var errStale = errors.New("the session is no longer idle on that turn")

// read marks the turn of a session read, for every page: only the turn it is
// idle on now, so a turn still running is never taken as read.
func (w *world) read(k, turn string) error {
	name, ok := machineOf(k)
	w.mu.Lock()
	defer w.mu.Unlock()
	t, known := w.machines[name]
	if !ok || !known {
		return errStale
	}
	s, ok := t.sessions[k]
	if !ok || s.State != "idle" || s.Turn == "" || s.Turn != turn {
		return errStale
	}
	if err := w.seen.mark(k, turn); err != nil {
		return err
	}
	w.publish(name)
	return nil
}

// join adds a page's stream and sends it everything: the machines, every
// session, then ready.
func (w *world) join(s *stream, hello any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.streams[s] = struct{}{}
	s.send("hello", mustJSON(hello))
	for _, n := range w.order {
		s.send("machine", mustJSON(w.machines[n].view))
	}
	for _, n := range w.order {
		views := w.views(n)
		keys := make([]string, 0, len(views))
		for k := range views {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.send("session", mustJSON(views[k]))
		}
	}
	s.send("ready", []byte("{}"))
}

func (w *world) leave(s *stream) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.streams, s)
}

// streamsOf returns the streams of a device, or of every device when id is
// empty.
func (w *world) streamsOf(id string) []*stream {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*stream
	for s := range w.streams {
		if id == "" || s.device == id {
			out = append(out, s)
		}
	}
	return out
}

// broadcast sends every page one frame. The caller holds the lock.
func (w *world) broadcast(event string, v any) { w.broadcastRaw(event, mustJSON(v)) }

func (w *world) broadcastRaw(event string, data []byte) {
	for s := range w.streams {
		s.send(event, data)
	}
}

// tell sends every page one frame.
func (w *world) tell(event string, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.broadcast(event, v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// stream is one page's event stream. Frames wait in a bounded queue; a page
// that falls behind is cut off and reconnects to a fresh start.
type stream struct {
	device string
	frames chan []byte
	done   chan struct{}
	once   sync.Once
}

const streamQueue = 512

func newStream(device string) *stream {
	return &stream{device: device, frames: make(chan []byte, streamQueue), done: make(chan struct{})}
}

func (s *stream) send(event string, data []byte) {
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	select {
	case s.frames <- frame:
	default:
		s.close()
	}
}

func (s *stream) close() { s.once.Do(func() { close(s.done) }) }

// seenTurns remembers the last turn read of each session, by the time it was
// marked, at most seenLimit: ids and turn ids only.
type seenTurns struct {
	path  string
	turns map[string]seenTurn
	now   func() time.Time
}

type seenTurn struct {
	Turn string    `json:"turn"`
	At   time.Time `json:"at"`
}

const seenLimit = 512

func loadSeen(dir string) (*seenTurns, error) {
	s := &seenTurns{path: filepath.Join(dir, "seen.json"), turns: map[string]seenTurn{}, now: time.Now}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.turns); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *seenTurns) turn(k string) string { return s.turns[k].Turn }

func (s *seenTurns) mark(k, turn string) error {
	s.turns[k] = seenTurn{Turn: turn, At: s.now().UTC()}
	if over := len(s.turns) - seenLimit; over > 0 {
		keys := make([]string, 0, len(s.turns))
		for k := range s.turns {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return s.turns[a].At.Compare(s.turns[b].At) })
		for _, k := range keys[:over] {
			delete(s.turns, k)
		}
	}
	b, err := json.Marshal(s.turns)
	if err != nil {
		return err
	}
	return replaceFile(s.path, b)
}

// replaceFile replaces path with b atomically, readable by its owner alone.
func replaceFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
