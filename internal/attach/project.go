package attach

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Projected is a pane Project wrote a session's projection to, as tmux
// showed it then.
type Projected struct {
	Pane tmux.Pane
	// Newer is true when the record read under the pane's lock showed a newer
	// state than the one Project was given: its own hook reports that state.
	Newer bool
	// Read is true when Options and Activity were read, with the first pane
	// of each server: the server's global options asked for, and its
	// clients' last input, zero when none is attached.
	Read     bool
	Options  map[string]string
	Activity time.Time
}

// Project writes the session's projection to every pane its record lists,
// one at a time, each under the pane's lock, from the record read there: it
// needs neither a pane listing nor the process table. Each write goes through
// a guard inside tmux, so it lands only while the pane holds the attachment
// the record lists, is alive, and still runs the program it was attached
// with; a Codex pane's title must also still show the session, as Check asks.
// A pane tmux shows holds the attachment no more (closed, its server gone,
// or its options naming another) leaves the record, under the pane's lock.
// One Project cannot write for now stays listed, for a full pass to judge.
// With options, the same invocation as each server's first pane reads those
// global options and the server's client activity. It returns the panes
// written and the listed panes it did not write.
func (env Env) Project(ctx context.Context, rec session.Record, options ...string) ([]Projected, []session.Shown, error) {
	var written []Projected
	var missed []session.Shown
	read := map[string]bool{}
	for _, s := range rec.Shown {
		ask := len(options) > 0 && !read[s.Server]
		err := Lock(ctx, env.Log.Dir, s.Server, s.Pane, func() error {
			latest, ok, err := env.Store.Get(ctx, rec.Agent, rec.ID)
			if err != nil || !ok || !latest.Lists(s) {
				return err
			}
			pr := ProjectionOf(latest)
			got, outcome := guardedWrite(ctx, s, latest, pr, ask, options)
			switch outcome {
			case wrote:
				got.Newer = pr != ProjectionOf(rec)
				read[s.Server] = read[s.Server] || ask
				written = append(written, got)
				return nil
			case gone:
				missed = append(missed, s)
				return env.unlistGone(ctx, rec.Agent, rec.ID, s)
			}
			missed = append(missed, s)
			return nil
		})
		if err != nil {
			return written, missed, err
		}
	}
	return written, missed, nil
}

// unlistGone stops the session listing s, which tmux showed holds its
// attachment no more. The caller holds the pane's lock, so no Attach of the
// pane runs meanwhile.
func (env Env) unlistGone(ctx context.Context, agent, id string, s session.Shown) error {
	return env.Store.Lock(ctx, agent, id, func() error {
		rec, ok, err := env.Store.Get(ctx, agent, id)
		if err != nil || !ok || !rec.Lists(s) {
			return err
		}
		rec.Unlist(s.Server, s.Pane)
		return env.Store.Put(ctx, rec)
	})
}

// outcome is what a guarded write found.
type outcome int

const (
	// unwritten: the pane may still hold the attachment, but was not
	// written: it is dead, another program runs in front, a Codex title
	// shows the session no more, or tmux did not answer as asked.
	unwritten outcome = iota
	wrote
	// gone: tmux showed the pane holds the attachment no more.
	gone
)

// guardRefused is printed by the guard's else branch, so a refusal is an
// explicit result rather than something inferred from the projection after a
// write that may already have landed.
const guardRefused = "tmag-guard-refused"

// guardedWrite sets pr on the listed pane s in one tmux invocation, only if
// tmux still shows the attachment s names on a live pane running the program
// it was attached with, then prints the pane; with ask it also reads the
// server's options and client activity.
//
// A Codex pane is written only while its title shows the session, as Check
// judges it. Its title is read and judged first, under the same pane lock,
// by an invocation that also has tmux keep that title on the pane; the
// write's guard asks that the title is still the one kept. tmux keeps and
// compares the title itself, so no title passes through a command line. A
// title that changed in between, as a spinner's does, is read and judged
// again, titleReads times at most.
func guardedWrite(ctx context.Context, s session.Shown, rec session.Record, pr Projection, ask bool, options []string) (Projected, outcome) {
	if !s.Valid() || !named(rec.Agent, rec.ID) {
		return Projected{}, unwritten
	}
	srv := tmux.Server{Socket: s.Server}
	if rec.Agent != proc.Codex {
		got, out, _ := guardedCall(ctx, srv, s, rec, pr, ask, options, false)
		return got, out
	}
	for range titleReads {
		p, err := keepTitle(ctx, srv, s.Pane)
		if err != nil {
			return Projected{}, absence(ctx, srv, s.Pane)
		}
		if !holds(p, s, rec) {
			return Projected{}, gone
		}
		if !titleShows(p, rec) {
			return Projected{}, unwritten
		}
		got, out, after := guardedCall(ctx, srv, s, rec, pr, ask, options, true)
		if out != unwritten || after == nil || after.Title == p.Title {
			return got, out
		}
	}
	return Projected{}, unwritten
}

// keepTitle reads the pane, and has tmux keep the title it reads on it, in
// one invocation.
func keepTitle(ctx context.Context, srv tmux.Server, pane string) (tmux.Pane, error) {
	out, err := srv.Run(ctx, "set-option", "-p", "-F", "-t", pane, tmux.OptionPrefix+optTitle, "#{pane_title}", ";",
		"display-message", "-p", "-t", pane, tmux.PaneFormat)
	if err != nil {
		return tmux.Pane{}, err
	}
	p, ok := srv.ParsePane(strings.TrimRight(string(out), "\n"))
	if !ok || p.ID != pane {
		return tmux.Pane{}, fmt.Errorf("pane %s: unexpected tmux output", pane)
	}
	return p, nil
}

// titleReads bounds how often a Codex pane's title is read and judged again
// for one write, when it changed under the write.
const titleReads = 3

// holds reports whether pane p holds the attachment s lists for rec.
func holds(p tmux.Pane, s session.Shown, rec session.Record) bool {
	a, ok := FromPane(p)
	return ok && a.Agent == rec.Agent && a.Session == rec.ID && a.PID == s.PID && a.Start == s.Start
}

// guardedCall is one guarded write. With title, the guard also asks that the
// pane's title is still the one keepTitle kept. It returns the pane as
// printed after the guard, when tmux printed it.
func guardedCall(ctx context.Context, srv tmux.Server, s session.Shown, rec session.Record, pr Projection, ask bool, options []string, title bool) (Projected, outcome, *tmux.Pane) {
	terms := []string{
		"#{==:#{" + tmux.OptionPrefix + optAgent + "}," + rec.Agent + "}",
		"#{==:#{" + tmux.OptionPrefix + optSession + "}," + rec.ID + "}",
		"#{==:#{" + tmux.OptionPrefix + optPID + "}," + strconv.Itoa(s.PID) + "}",
		"#{==:#{" + tmux.OptionPrefix + optStart + "}," + s.Start + "}",
		"#{?pane_dead,0,1}",
		"#{==:#{pane_current_command},#{" + tmux.OptionPrefix + optCmd + "}}",
	}
	if title {
		terms = append(terms, "#{==:#{pane_title},#{"+tmux.OptionPrefix+optTitle+"}}")
	}
	accepted := projectionCommands(s.Pane, pr) + " ; display-message -p -t " + s.Pane + " ok"
	refused := "display-message -p -t " + s.Pane + " " + guardRefused
	args := []string{"if", "-F", "-t", s.Pane, conjunction(terms...), accepted, refused, ";",
		"display-message", "-p", "-t", s.Pane, tmux.PaneFormat}
	if ask {
		args = append(append(args, ";"), tmux.OptionCommands(options...)...)
		args = append(args, ";", "list-clients", "-F", "#{client_activity}")
	}
	raw, err := srv.Run(ctx, args...)
	lines := strings.SplitN(string(raw), "\n", 3)
	if err != nil || len(lines) < 2 {
		return Projected{}, absence(ctx, srv, s.Pane), nil
	}
	pane, ok := srv.ParsePane(lines[1])
	if !ok || pane.ID != s.Pane {
		// tmux prints a target it cannot find as an empty pane.
		return Projected{}, absence(ctx, srv, s.Pane), nil
	}
	if !holds(pane, s, rec) {
		return Projected{}, gone, &pane
	}
	if lines[0] != "ok" || projectionFromPane(pane) != pr {
		return Projected{}, unwritten, &pane
	}
	got := Projected{Pane: pane}
	if ask {
		values, err := tmux.ParseOptions(raw, options...)
		if err != nil {
			return Projected{}, unwritten, &pane
		}
		got.Read, got.Options = true, values
		got.Activity, _ = tmux.ClientActivity(tmux.AfterOptions(raw))
	}
	return got, wrote, &pane
}

// titleShows reports whether a Codex pane's title lets it show rec, as Check
// decides: it shows one of the session's threads, or, for a process
// attachment, no thread at all.
func titleShows(p tmux.Pane, rec session.Record) bool {
	if _, ok := shows(p.Title, rec); ok {
		return true
	}
	a, _ := FromPane(p)
	return a.Evidence != TitleID && len(TitleIDs(p.Title)) == 0
}

// absence is what a failed tmux call proves of a pane: gone when its server
// answers without it, or no server listens on its socket any more.
func absence(ctx context.Context, srv tmux.Server, pane string) outcome {
	if ctx.Err() != nil {
		return unwritten
	}
	out, err := srv.Run(ctx, "list-panes", "-a", "-F", "#{pane_id}")
	if err == nil {
		if slices.Contains(strings.Fields(string(out)), pane) {
			return unwritten
		}
		return gone
	}
	if ctx.Err() == nil && !srv.Listening(ctx) {
		return gone
	}
	return unwritten
}

// conjunction nests tmux's binary && so every predicate is checked; a flat
// #{&&:a,b,c} does not mean what it looks like.
func conjunction(terms ...string) string {
	out := terms[len(terms)-1]
	for i := len(terms) - 2; i >= 0; i-- {
		out = "#{&&:" + terms[i] + "," + out + "}"
	}
	return out
}

// projectionCommands sets one projection in a tmux command string, unsetting
// what the projection does not carry.
func projectionCommands(pane string, pr Projection) string {
	var cmds []string
	opt := func(k, v string) {
		if v == "" {
			cmds = append(cmds, "set-option -p -u -t "+pane+" "+tmux.OptionPrefix+k)
		} else {
			cmds = append(cmds, fmt.Sprintf("set-option -p -t %s %s%s %s", pane, tmux.OptionPrefix, k, v))
		}
	}
	opt(optState, pr.State)
	opt(optKind, pr.Kind)
	since := ""
	if !pr.Since.IsZero() {
		since = strconv.FormatInt(pr.Since.Unix(), 10)
	}
	opt(optSince, since)
	return strings.Join(cmds, " ; ")
}
