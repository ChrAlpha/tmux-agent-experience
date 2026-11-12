package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// seenLimit bounds the turns the gateway remembers; a person has not seen
// more sessions than this in a week.
const seenLimit = 4096

// seenForget is how long a session no machine has shown leaves a trace.
const seenForget = 7 * 24 * time.Hour

// seenFile is where the gateway keeps seen turns between runs.
const seenFile = "seen.json"

// seenSaveEvery is how often a touch alone rewrites the file.
const seenSaveEvery = time.Minute

// Sections a session sorts into on the page.
const (
	SectionInbox   = "inbox"
	SectionWorking = "working"
	SectionSettled = "settled"
	SectionUnknown = "unknown"
)

// tracker remembers which turn of which session the person has seen. It
// keeps ids and times only, never a reply.
type tracker struct {
	mu   sync.Mutex
	now  func() time.Time
	seen map[string]seenTurn
	// live names every session a machine shows now, or is nil until every
	// machine has shown its state.
	live func() map[string]bool

	path  string
	saved time.Time
	dirty bool
}

type seenTurn struct {
	machine string
	agent   string
	id      string
	turn    string
	shown   time.Time
}

type seenRecord struct {
	Machine string    `json:"machine"`
	Agent   string    `json:"agent"`
	ID      string    `json:"id"`
	Turn    string    `json:"turn"`
	Shown   time.Time `json:"shown"`
}

type seenDocument struct {
	Version int          `json:"version"`
	Seen    []seenRecord `json:"seen"`
}

func newTracker() *tracker {
	return &tracker{now: time.Now, seen: map[string]seenTurn{}}
}

// loadTracker reads the turns a previous gateway saw. A missing file is an
// empty history.
func loadTracker(dir string) (*tracker, error) {
	t := newTracker()
	t.path = filepath.Join(dir, seenFile)
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var doc seenDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Version != 1 {
		return nil, errors.New("unsupported seen-turn store")
	}
	for _, r := range doc.Seen {
		if r.Turn == "" {
			continue
		}
		t.seen[sessionID(r.Machine, r.Agent, r.ID)] = seenTurn{
			machine: r.Machine, agent: r.Agent, id: r.ID, turn: r.Turn, shown: r.Shown,
		}
	}
	if t.trimLocked() {
		t.dirty = true
		t.saveLocked(true)
	}
	return t, nil
}

func sessionID(machine, agent, id string) string { return machine + "/" + agent + "/" + id }

// touch says a machine still shows these sessions.
func (t *tracker) touch(machine string, sessions []Session) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range sessions {
		k := sessionID(machine, s.Agent, s.ID)
		if v, ok := t.seen[k]; ok {
			v.shown = now
			t.seen[k] = v
			t.dirty = true
		}
	}
	if t.pruneLocked(now) {
		t.dirty = true
	}
	t.saveLocked(false)
}

// mark records a turn as seen and returns it, or "" when there is none.
func (t *tracker) mark(machine, agent, id, turn string) string {
	if turn == "" {
		return ""
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[sessionID(machine, agent, id)] = seenTurn{machine: machine, agent: agent, id: id, turn: turn, shown: now}
	t.dirty = true
	t.pruneLocked(now)
	t.saveLocked(true)
	return turn
}

// snapshot returns the seen turn of every remembered session.
func (t *tracker) snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pruneLocked(t.now()) {
		t.dirty = true
		t.saveLocked(true)
	}
	out := make(map[string]string, len(t.seen))
	for k, v := range t.seen {
		out[k] = v.turn
	}
	return out
}

// flush writes any change a touch left for later.
func (t *tracker) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saveLocked(true)
}

// pruneLocked refreshes the last-shown time of every session a machine
// shows, drops old turns no machine shows and keeps the newest seenLimit of
// them. While the machines' state is unknown, only the limit applies.
func (t *tracker) pruneLocked(now time.Time) (changed bool) {
	live := map[string]bool{}
	if t.live != nil {
		live = t.live()
	}
	if live != nil {
		for k, v := range t.seen {
			if live[k] {
				if !v.shown.Equal(now) {
					v.shown = now
					t.seen[k] = v
					changed = true
				}
				continue
			}
			if now.Sub(v.shown) > seenForget {
				delete(t.seen, k)
				changed = true
			}
		}
	}
	return t.trimLocked() || changed
}

// trimLocked keeps the newest seenLimit turns.
func (t *tracker) trimLocked() (changed bool) {
	if len(t.seen) > seenLimit {
		type pair struct {
			k string
			t time.Time
		}
		all := make([]pair, 0, len(t.seen))
		for k, v := range t.seen {
			all = append(all, pair{k, v.shown})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, p := range all[:len(all)-seenLimit] {
			delete(t.seen, p.k)
		}
		changed = true
	}
	return changed
}

// saveLocked replaces the file atomically. A touch alone saves at most once a
// minute; a seen turn and a shutdown save at once.
func (t *tracker) saveLocked(force bool) {
	if t.path == "" || !t.dirty {
		return
	}
	now := t.now()
	if !force && !t.saved.IsZero() && now.Sub(t.saved) < seenSaveEvery {
		return
	}
	doc := seenDocument{Version: 1, Seen: make([]seenRecord, 0, len(t.seen))}
	for _, v := range t.seen {
		doc.Seen = append(doc.Seen, seenRecord{Machine: v.machine, Agent: v.agent, ID: v.id, Turn: v.turn, Shown: v.shown})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".seen-")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return
	}
	if os.Rename(f.Name(), t.path) != nil {
		return
	}
	t.saved = now
	t.dirty = false
}

// isSeen reports whether the session's current turn was seen.
func (t *tracker) isSeen(machine string, s Session) bool {
	if s.Turn == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.seen[sessionID(machine, s.Agent, s.ID)]
	return ok && v.turn == s.Turn
}

// Section names the inbox group a session belongs to.
func Section(s Session, seen bool) string { return section(s, seen) }

// section sorts one session into the page's inbox.
func section(s Session, seen bool) string {
	switch {
	case s.State == "blocked":
		return SectionInbox
	case s.State == "idle" && s.Turn != "" && !seen:
		return SectionInbox
	case s.State == "working":
		return SectionWorking
	case s.State == "idle":
		return SectionSettled
	}
	return SectionUnknown
}

// waiting is how long the session has waited: its state change, or its
// oldest open request when blocked.
func waiting(s Session) time.Time {
	if s.State == "blocked" && len(s.Requests) > 0 {
		oldest := s.Requests[0].Since
		for _, q := range s.Requests[1:] {
			if q.Since.Before(oldest) {
				oldest = q.Since
			}
		}
		return oldest
	}
	if s.Since != nil {
		return *s.Since
	}
	return time.Time{}
}
