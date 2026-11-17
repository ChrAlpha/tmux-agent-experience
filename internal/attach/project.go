package attach

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// with. A refused pane leaves the record, under the pane's lock. With options,
// the same invocation as each server's first pane reads those global options
// and the server's client activity. It returns the panes written and those
// refused.
func (env Env) Project(ctx context.Context, rec session.Record, options ...string) ([]Projected, []session.Shown, error) {
	var written []Projected
	var refused []session.Shown
	read := map[string]bool{}
	for _, s := range rec.Shown {
		ask := len(options) > 0 && !read[s.Server]
		err := Lock(ctx, env.Log.Dir, s.Server, s.Pane, func() error {
			latest, ok, err := env.Store.Get(ctx, rec.Agent, rec.ID)
			if err != nil || !ok || !latest.Lists(s) {
				return err
			}
			pr := ProjectionOf(latest)
			got, ok := guardedWrite(ctx, s, rec.Agent, rec.ID, pr, ask, options)
			if !ok {
				refused = append(refused, s)
				return env.unlistRefused(ctx, rec.Agent, rec.ID, s)
			}
			got.Newer = pr != ProjectionOf(rec)
			read[s.Server] = read[s.Server] || ask
			written = append(written, got)
			return nil
		})
		if err != nil {
			return written, refused, err
		}
	}
	return written, refused, nil
}

// unlistRefused stops the session listing s, whose guard refused it. The
// caller holds the pane's lock, so no Attach of the pane runs meanwhile.
func (env Env) unlistRefused(ctx context.Context, agent, id string, s session.Shown) error {
	return env.Store.Lock(ctx, agent, id, func() error {
		rec, ok, err := env.Store.Get(ctx, agent, id)
		if err != nil || !ok || !rec.Lists(s) {
			return err
		}
		rec.Unlist(s.Server, s.Pane)
		return env.Store.Put(ctx, rec)
	})
}

// guardRefused is printed by the guard's else branch, so a refusal is an
// explicit result rather than something inferred from the projection after a
// write that may already have landed.
const guardRefused = "tmag-guard-refused"

// guardedWrite sets pr on the listed pane s in one tmux invocation, only if
// tmux still shows the attachment s names on a live pane running the program
// it was attached with, then prints the pane; with ask it also reads the
// server's options and client activity. False means the guard refused, or the
// pane did not show the write.
func guardedWrite(ctx context.Context, s session.Shown, agent, id string, pr Projection, ask bool, options []string) (Projected, bool) {
	if !s.Valid() || !named(agent, id) {
		return Projected{}, false
	}
	srv := tmux.Server{Socket: s.Server}
	guard := conjunction(
		"#{==:#{"+tmux.OptionPrefix+optAgent+"},"+agent+"}",
		"#{==:#{"+tmux.OptionPrefix+optSession+"},"+id+"}",
		"#{==:#{"+tmux.OptionPrefix+optPID+"},"+strconv.Itoa(s.PID)+"}",
		"#{==:#{"+tmux.OptionPrefix+optStart+"},"+s.Start+"}",
		"#{?pane_dead,0,1}",
		"#{==:#{pane_current_command},#{"+tmux.OptionPrefix+optCmd+"}}",
	)
	accepted := projectionCommands(s.Pane, pr) + " ; display-message -p -t " + s.Pane + " ok"
	refused := "display-message -p -t " + s.Pane + " " + guardRefused
	args := []string{"if", "-F", "-t", s.Pane, guard, accepted, refused, ";",
		"display-message", "-p", "-t", s.Pane, tmux.PaneFormat}
	if ask {
		args = append(append(args, ";"), tmux.OptionCommands(options...)...)
		args = append(args, ";", "list-clients", "-F", "#{client_activity}")
	}
	raw, _ := srv.Run(ctx, args...)
	lines := strings.SplitN(string(raw), "\n", 3)
	if len(lines) < 2 || lines[0] != "ok" {
		return Projected{}, false
	}
	pane, ok := srv.ParsePane(lines[1])
	if !ok || pane.ID != s.Pane {
		return Projected{}, false
	}
	a, ok := FromPane(pane)
	if !ok || a.Agent != agent || a.Session != id || a.PID != s.PID || a.Start != s.Start || projectionFromPane(pane) != pr {
		return Projected{}, false
	}
	got := Projected{Pane: pane}
	if ask {
		values, err := tmux.ParseOptions(raw, options...)
		if err != nil {
			return Projected{}, false
		}
		got.Read, got.Options = true, values
		got.Activity, _ = tmux.ClientActivity(tmux.AfterOptions(raw))
	}
	return got, true
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
