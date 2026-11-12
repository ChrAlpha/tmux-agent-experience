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

// seenForget is how long an absent session leaves a trace.
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
	mu      sync.Mutex
	now     func() time.Time
	seen    map[string]seenTurn
	present map[string]map[string]bool // machine -> sessionKey(agent, id)

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
	absent  time.Time // zero while present, or when absence was never marked
}

type seenRecord struct {
	Machine string     `json:"machine"`
	Agent   string     `json:"agent"`
	ID      string     `json:"id"`
	Turn    string     `json:"turn"`
	Shown   time.Time  `json:"shown"`
	Absent  *time.Time `json:"absent,omitempty"`
}

type seenDocument struct {
	Version int          `json:"version"`
	Seen    []seenRecord `json:"seen"`
}

func newTracker() *tracker {
	return &tracker{now: time.Now, seen: map[string]seenTurn{}, present: map[string]map[string]bool{}}
}

// setNow changes the clock under the tracker's lock.
func (t *tracker) setNow(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}

// configure names the machines this gateway follows. Markers of machines it
// no longer follows become absent now, unless they already were.
func (t *tracker) configure(machines []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	configured := make(map[string]bool, len(machines))
	present := make(map[string]map[string]bool, len(machines))
	for _, m := range machines {
		configured[m] = true
		if old, ok := t.present[m]; ok {
			present[m] = old
		} else {
			present[m] = map[string]bool{}
		}
	}
	t.present = present
	changed := false
	for k, v := range t.seen {
		if !configured[v.machine] && v.absent.IsZero() {
			v.absent = now
			t.seen[k] = v
			changed = true
		}
	}
	if t.pruneLocked(now) {
		changed = true
	}
	if changed {
		t.dirty = true
		t.saveLocked(true)
	}
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
		v := seenTurn{machine: r.Machine, agent: r.Agent, id: r.ID, turn: r.Turn, shown: r.Shown}
		if r.Absent != nil {
			v.absent = *r.Absent
		}
		t.seen[sessionID(r.Machine, r.Agent, r.ID)] = v
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
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.present[machine] == nil {
		t.present[machine] = map[string]bool{}
	}
	changed := false
	for _, s := range sessions {
		t.present[machine][sessionKey(s.Agent, s.ID)] = true
		k := sessionID(machine, s.Agent, s.ID)
		if v, ok := t.seen[k]; ok {
			if !v.absent.IsZero() {
				v.absent = time.Time{}
				changed = true
			}
			v.shown = now
			t.seen[k] = v
			t.dirty = true
		}
	}
	if t.pruneLocked(now) {
		changed = true
	}
	t.saveLocked(changed)
}

// observe says a machine's snapshot shows exactly these sessions. A marker
// of this machine, for a session the snapshot lacks, becomes absent now.
func (t *tracker) observe(machine string, sessions []Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	live := make(map[string]bool, len(sessions))
	changed := false
	for _, s := range sessions {
		live[sessionKey(s.Agent, s.ID)] = true
		k := sessionID(machine, s.Agent, s.ID)
		if v, ok := t.seen[k]; ok {
			if !v.absent.IsZero() {
				v.absent = time.Time{}
				changed = true
			}
			v.shown = now
			t.seen[k] = v
			t.dirty = true
		}
	}
	for k, v := range t.seen {
		if v.machine == machine && !live[sessionKey(v.agent, v.id)] && v.absent.IsZero() {
			v.absent = now
			t.seen[k] = v
			t.dirty = true
			changed = true
		}
	}
	t.present[machine] = live
	if t.pruneLocked(now) {
		changed = true
	}
	t.saveLocked(changed)
}

// leave records when a machine stopped showing these sessions, so absence
// counts from that moment.
func (t *tracker) leave(machine string, sessions []Session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	changed := false
	for _, s := range sessions {
		if t.present[machine] != nil {
			delete(t.present[machine], sessionKey(s.Agent, s.ID))
		}
		k := sessionID(machine, s.Agent, s.ID)
		if v, ok := t.seen[k]; ok && v.absent.IsZero() {
			v.absent = now
			t.seen[k] = v
			t.dirty = true
			changed = true
		}
	}
	if t.pruneLocked(now) {
		changed = true
	}
	t.saveLocked(changed)
}

// mark records a turn as seen and returns it, or "" when there is none or
// its session is no longer present on its machine.
func (t *tracker) mark(machine, agent, id, turn string) string {
	if turn == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.present[machine][sessionKey(agent, id)] {
		return ""
	}
	now := t.now()
	k := sessionID(machine, agent, id)
	t.seen[k] = seenTurn{machine: machine, agent: agent, id: id, turn: turn, shown: now}
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

// pruneLocked drops turns absent for a week and keeps the newest seenLimit
// of them. A present marker, or one never marked absent, stays.
func (t *tracker) pruneLocked(now time.Time) (changed bool) {
	for k, v := range t.seen {
		if !v.absent.IsZero() && now.Sub(v.absent) > seenForget {
			delete(t.seen, k)
			changed = true
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
		r := seenRecord{Machine: v.machine, Agent: v.agent, ID: v.id, Turn: v.turn, Shown: v.shown}
		if !v.absent.IsZero() {
			at := v.absent
			r.Absent = &at
		}
		doc.Seen = append(doc.Seen, r)
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
