package attach

import (
	"context"
	"crypto/sha256"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Evidence that a client shows a session.
const (
	// Process: the session's hooks descend from the client, which is its
	// pane's own agent process.
	Process = "process"
	// TitleID: the client is Codex and its terminal title shows the thread
	// id, through the thread-id item of [tui] terminal_title.
	TitleID = "title-id"
)

// Views of an attached client.
const (
	// ViewMain: the client shows the session's main agent.
	ViewMain = "main"
	// ViewSubagent: the client shows one of the session's subagents, where
	// Codex accepts no input.
	ViewSubagent = "subagent"
)

// Attachment is the session a pane's client shows, as stored on the pane.
type Attachment struct {
	Agent   string
	Session string
	// PID and Start identify the client process the attachment belongs to.
	PID      int
	Start    string
	Evidence string
}

// keeps reports whether a stands against b: it is the same attachment, or
// a process proof of the client and session b names, which a title
// inference never replaces: a process proof lasts as long as its client.
func (a Attachment) keeps(b Attachment) bool {
	return a.Agent == b.Agent && a.Session == b.Session && a.PID == b.PID && a.Start == b.Start &&
		(a.Evidence == b.Evidence || a.Evidence == Process)
}

// Pane options. The attachment and the display projection share the pane,
// and both vanish with it. The projection (state, kind, since, cmd) is a
// public, read-only tmux interface: formats and scripts read it without tmag.
const (
	optAgent    = "agent"
	optSession  = "session"
	optPID      = "pid"
	optStart    = "start"
	optEvidence = "evidence"
	optState    = "state"
	optKind     = "kind"
	optSince    = "since"
	optCmd      = "cmd"
)

// FromPane reads the attachment stored on a pane: only one tmag could have
// written, naming sessions as the session store does, so that every event an
// attachment logs is about a session tmag can name. Anything else on the pane
// is no attachment.
func FromPane(p tmux.Pane) (Attachment, bool) {
	o := p.Options
	a := Attachment{Agent: o[optAgent], Session: o[optSession], Start: o[optStart], Evidence: o[optEvidence]}
	a.PID, _ = strconv.Atoi(o[optPID])
	if !named(a.Agent, a.Session) || a.PID <= 0 || (a.Evidence != Process && a.Evidence != TitleID) {
		return Attachment{}, false
	}
	return a, true
}

// named reports whether agent and id name a session as the store does.
func named(agent, id string) bool {
	return proc.IsAgent(agent) && session.ValidID(id)
}

// Projection is what tmux displays for an attached pane.
type Projection struct {
	State string
	Kind  string
	Since time.Time
}

// ProjectionOf is the display of a session.
func ProjectionOf(rec session.Record) Projection {
	return Projection{State: rec.State(), Kind: rec.Kind(), Since: rec.Since}
}

func projectionFromPane(p tmux.Pane) Projection {
	o := p.Options
	pr := Projection{State: o[optState], Kind: o[optKind]}
	if n, err := strconv.ParseInt(o[optSince], 10, 64); err == nil {
		pr.Since = time.Unix(n, 0).UTC()
	}
	return pr
}

// write stores an attachment and its projection on the pane. cmd is the
// pane's current command, which the display format checks so that a record
// left behind by an exited agent shows nothing once another program runs.
func write(ctx context.Context, srv tmux.Server, pane, cmd string, a Attachment, pr Projection) error {
	set := map[string]string{
		optAgent: a.Agent, optSession: a.Session, optPID: strconv.Itoa(a.PID),
		optEvidence: a.Evidence, optState: pr.State, optCmd: cmd,
	}
	var unset []string
	opt := func(k, v string) {
		if v != "" {
			set[k] = v
		} else {
			unset = append(unset, k)
		}
	}
	opt(optStart, a.Start)
	opt(optKind, pr.Kind)
	since := ""
	if !pr.Since.IsZero() {
		since = strconv.FormatInt(pr.Since.Unix(), 10)
	}
	opt(optSince, since)
	return srv.SetPaneOptions(ctx, pane, set, unset)
}

func writeProjection(ctx context.Context, srv tmux.Server, pane string, pr Projection) error {
	set := map[string]string{optState: pr.State}
	var unset []string
	if pr.Kind != "" {
		set[optKind] = pr.Kind
	} else {
		unset = append(unset, optKind)
	}
	if !pr.Since.IsZero() {
		set[optSince] = strconv.FormatInt(pr.Since.Unix(), 10)
	} else {
		unset = append(unset, optSince)
	}
	return srv.SetPaneOptions(ctx, pane, set, unset)
}

// Lock runs fn holding the pane's lock, a stripe of the state directory's
// lock pool shared by every tmag process. A pane's attachment and projection
// change only under it, and whoever also takes a session's lock takes the
// pane's first.
func Lock(ctx context.Context, stateDir, server, pane string, fn func() error) error {
	sum := sha256.Sum256([]byte(server + "\x00" + pane))
	return lockfile.Stripe(ctx, stateDir, "p", sum, fn)
}

// titleIDLen is how many leading characters of a thread id the Codex 0.166
// thread-id title item shows before "...".
const titleIDLen = 29

// titleIDRE is the thread-id item exactly: a UUID cut after titleIDLen
// characters. Thread names, which come from the user's prompts, are other
// items; one would have to be precisely this to pass for an id.
var titleIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{5}\.\.\.$`)

// TitleIDs returns the thread-id prefixes a Codex title shows. Items are
// joined by " | "; the activity indicator joins its neighbours by a space,
// so an item is an id when, without activity glyphs, it is exactly one.
func TitleIDs(title string) []string {
	var out []string
	for _, item := range strings.Split(title, " | ") {
		var words []string
		for _, word := range strings.Fields(item) {
			if strings.IndexFunc(word, func(r rune) bool { return !isActivity(r) }) >= 0 {
				words = append(words, word)
			}
		}
		if len(words) == 1 && titleIDRE.MatchString(words[0]) {
			out = append(out, strings.TrimSuffix(words[0], "..."))
		}
	}
	return out
}

// isActivity matches the title's activity glyphs: Braille spinner frames and
// the dictation dot.
func isActivity(r rune) bool { return (r >= 0x2800 && r <= 0x28ff) || r == '●' }

// shows reports how a Codex title relates to a session: the view it shows,
// or ok false when it shows no id of the session's.
func shows(title string, rec session.Record) (view string, ok bool) {
	for _, id := range TitleIDs(title) {
		if strings.HasPrefix(rec.ID, id) {
			return ViewMain, true
		}
		for _, sub := range rec.Subagents {
			if strings.HasPrefix(sub, id) {
				return ViewSubagent, true
			}
		}
	}
	return "", false
}

// Check reports whether an attachment still holds in the world, and which
// view the client shows: the client process is the one it was made for,
// and a title-id client's title still shows the session.
func (w *World) Check(p tmux.Pane, a Attachment, rec session.Record) (view string, ok bool) {
	c, found := w.ClientIn(p)
	if !found || c.Agent != a.Agent || c.Process.PID != a.PID || (a.Start != "" && c.Process.Start != a.Start) {
		return "", false
	}
	if rec.Agent != a.Agent || rec.ID != a.Session {
		return "", false
	}
	if c.Agent == proc.Codex {
		if view, ok := shows(p.Title, rec); ok {
			return view, true
		}
		if a.Evidence == TitleID || len(TitleIDs(p.Title)) > 0 {
			// No longer shown, or another thread is.
			return "", false
		}
	}
	return ViewMain, true
}
