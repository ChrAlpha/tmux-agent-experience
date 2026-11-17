package hook

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/notify"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// matchingHint reports the record's pane hint when the hook's own host is the
// terminal client that hint was proven for.
func matchingHint(rec session.Record, host proc.Host, hostKnown bool) (session.PaneHint, bool) {
	if !hostKnown || host.Role != proc.Terminal || rec.Pane == nil {
		return session.PaneHint{}, false
	}
	h := *rec.Pane
	if h.PID != host.Process.PID || h.Start != host.Process.Start {
		return session.PaneHint{}, false
	}
	return h, true
}

// guardRefused is printed by the guard's else branch, so a refusal is an
// explicit result rather than something inferred from the projection after a
// write that may already have landed.
const guardRefused = "tmag-guard-refused"

// writeProjection is writeHintProjection, replaceable by tests to stage the
// order of two projection writers without a real tmux.
var writeProjection = writeHintProjection

// projectHint is the fast path's projection writer. It holds the pane's lock
// across the latest-record read and the guarded tmux invocation, so an older
// hook cannot overwrite a newer projection and attach.Show cannot race it. A
// false result means the hint was no longer current or the guard refused; the
// caller then takes the full proof path. A refusal clears the hint while the
// pane lock is held, taking the session lock second.
func projectHint(ctx context.Context, env attach.Env, agent, id string, hint session.PaneHint, alert bool) (session.Record, tmux.Pane, string, string, bool, bool, bool, error) {
	var latest session.Record
	var pane tmux.Pane
	var enabled, notifier string
	present, siblings, written := false, false, false
	err := attach.Lock(ctx, env.Log.Dir, hint.Server, hint.ID, func() error {
		rec, found, err := env.Store.Get(ctx, agent, id)
		if err != nil || !found {
			return err
		}
		latest = rec
		if !session.SamePaneHint(rec.Pane, &hint) {
			return nil
		}
		p, e, n, at, sib, ok := writeProjection(ctx, *rec.Pane, agent, id, attach.ProjectionOf(rec), alert)
		if !ok {
			return clearHint(ctx, env.Store, agent, id, hint)
		}
		pane, enabled, notifier, present, siblings, written = p, e, n, at, sib, true
		return nil
	})
	return latest, pane, enabled, notifier, present, siblings, written, err
}

// writeHintProjection updates one hinted pane only if tmux still shows the
// attachment the hint was proven for, and in the same invocation lists the
// panes on that server whose options name the session. When alert is set it
// also reads the alert and notify options losslessly and, after them, the
// clients' activity, so a finishing hook needs no second tmux process. The
// caller holds the pane's lock. A false result means the hint no longer
// holds.
func writeHintProjection(ctx context.Context, hint session.PaneHint, agent, id string, pr attach.Projection, alert bool) (tmux.Pane, string, string, bool, bool, bool) {
	if !hint.Valid() || !session.ValidID(id) || !proc.IsAgent(agent) {
		return tmux.Pane{}, "", "", false, false, false
	}
	srv := tmux.Server{Socket: hint.Server}
	guard := conjunction(
		fmt.Sprintf("#{==:#{@tmag_agent},%s}", agent),
		fmt.Sprintf("#{==:#{@tmag_session},%s}", id),
		fmt.Sprintf("#{==:#{@tmag_pid},%d}", hint.PID),
		fmt.Sprintf("#{==:#{@tmag_start},%s}", hint.Start),
		"#{?pane_dead,0,1}",
	)
	// Pane ids reach this format only through #{pane_id}: tmux runs strftime
	// over a display format, so a literal %N would be corrupted.
	siblings := fmt.Sprintf("#{S:#{W:#{P:#{?#{&&:#{==:#{@tmag_agent},%s},#{==:#{@tmag_session},%s}},#{pane_id} ,}}}}", agent, id)
	format := strings.Join([]string{
		"#{@tmag_agent}", "#{@tmag_session}", "#{@tmag_pid}", "#{@tmag_start}",
		"#{@tmag_state}", "#{@tmag_kind}", "#{@tmag_since}",
		"#{pane_tty}", "#{&&:#{session_attached},#{&&:#{window_active},#{pane_active}}}",
		"#{session_name}", "#{window_index}", "#{pane_index}", siblings,
	}, "\t")
	accepted := projectionCommands(hint.ID, pr) + " ; display-message -p -t " + hint.ID + " ok"
	refused := "display-message -p -t " + hint.ID + " " + guardRefused
	args := []string{"if", "-F", "-t", hint.ID, guard, accepted, refused, ";",
		"display-message", "-p", "-t", hint.ID, format}
	if alert {
		args = append(args, ";", "show-option", "-gq", notify.Option, ";", "show-option", "-gq", "@tmag-alert",
			";", "display-message", "-p", "$TMAG_OPTION_PRINT", ";", "list-clients", "-F", "#{client_activity}")
	}
	raw, _ := srv.Run(ctx, args...)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 2 || lines[0] != "ok" {
		return tmux.Pane{}, "", "", false, false, false
	}
	f := strings.Split(lines[1], "\t")
	if len(f) != 13 {
		return tmux.Pane{}, "", "", false, false, false
	}
	if f[0] != agent || f[1] != id || f[2] != strconv.Itoa(hint.PID) || f[3] != hint.Start ||
		f[4] != pr.State || f[5] != pr.Kind || f[6] != sinceString(pr.Since) {
		return tmux.Pane{}, "", "", false, false, false
	}
	seen := map[string]bool{hint.ID: true}
	other := false
	for _, id := range strings.Fields(f[12]) {
		if !seen[id] && strings.HasPrefix(id, "%") {
			seen[id] = true
			other = true
		}
	}
	win, _ := strconv.Atoi(f[10])
	idx, _ := strconv.Atoi(f[11])
	var enabled, notifier string
	present := false
	if alert {
		values, err := tmux.ParseOptions(raw, notify.Option, "@tmag-alert")
		if err != nil {
			return tmux.Pane{}, "", "", false, false, false
		}
		notifier, enabled = values[notify.Option], values["@tmag-alert"]
		activity := ""
		for i, line := range lines {
			if strings.TrimSpace(strings.TrimPrefix(line, `\`)) == "$TMAG_OPTION_PRINT" {
				activity = strings.Join(lines[i+1:], "\n")
				break
			}
		}
		if last, ok := tmux.ClientActivity([]byte(activity)); ok {
			present = time.Since(last) < notify.Present
		}
	}
	return tmux.Pane{
		Server: hint.Server, ID: hint.ID, Session: f[9], Window: win, Index: idx,
		TTY: f[7], Watched: f[8] == "1",
	}, enabled, notifier, present, other, true
}

// conjunction nests tmux's binary && so every predicate is checked; a flat
// #{&&:a,b,c} does not mean what it looks like.
func conjunction(terms ...string) string {
	if len(terms) == 0 {
		return "1"
	}
	out := terms[len(terms)-1]
	for i := len(terms) - 2; i >= 0; i-- {
		out = "#{&&:" + terms[i] + "," + out + "}"
	}
	return out
}

// projectionCommands sets one projection in a tmux command string, unsetting
// what the projection does not carry.
func projectionCommands(pane string, pr attach.Projection) string {
	var cmds []string
	set := func(k, v string) {
		cmds = append(cmds, "set-option -p -t "+pane+" "+tmux.OptionPrefix+k+" "+v)
	}
	unset := func(k string) {
		cmds = append(cmds, "set-option -p -u -t "+pane+" "+tmux.OptionPrefix+k)
	}
	if pr.State == "" {
		unset("state")
	} else {
		set("state", pr.State)
	}
	if pr.Kind == "" {
		unset("kind")
	} else {
		set("kind", pr.Kind)
	}
	if since := sinceString(pr.Since); since == "" {
		unset("since")
	} else {
		set("since", since)
	}
	return strings.Join(cmds, " ; ")
}

func sinceString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.Unix(), 10)
}

// clearHint removes the hint after its guarded write failed, only if the
// record still carries that exact hint.
func clearHint(ctx context.Context, store session.Store, agent, id string, hint session.PaneHint) error {
	return store.Lock(ctx, agent, id, func() error {
		rec, ok, err := store.Get(ctx, agent, id)
		if err != nil || !ok {
			return err
		}
		if !session.SamePaneHint(rec.Pane, &hint) {
			return nil
		}
		rec.Pane = nil
		return store.Put(ctx, rec)
	})
}
