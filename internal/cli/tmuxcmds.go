package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// reconciled brings the panes of one tmux server, or of all when server is
// empty, in line with the sessions, and returns the view of those panes and
// of the sessions they show. tmux's own displays (pane borders, the picker)
// read pane options directly, where nothing else checks them.
func (a *app) reconciled(ctx context.Context, server string) (query.View, *attach.World, error) {
	w, err := attach.Load(ctx)
	if err != nil {
		return query.View{}, nil, err
	}
	socket := ""
	if server != "" {
		socket = tmux.ServerByName(server).Socket
		w.Panes = slices.DeleteFunc(w.Panes, func(p tmux.Pane) bool { return p.Server != socket })
	}
	if err := attach.Reconcile(ctx, attach.Env{Log: a.log, Store: a.store()}, w); err != nil {
		return query.View{}, nil, err
	}
	v, fresh, err := query.Load(ctx, a.store())
	if err != nil || socket == "" {
		return v, fresh, err
	}
	v.Panes = slices.DeleteFunc(v.Panes, func(p query.Pane) bool { return p.Server != socket })
	v.Sessions = slices.DeleteFunc(v.Sessions, func(s query.Session) bool {
		return !slices.ContainsFunc(s.Panes, func(p query.Shown) bool { return p.Server == socket })
	})
	return v, fresh, nil
}

// cmdPrune reconciles panes of one server or all. The picker runs it first
// so that the tree it filters on pane options is current. It prints nothing.
func (a *app) cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	server := fs.String("server", "", "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = fmt.Errorf("unexpected argument %q", pos[0])
	}
	if err != nil {
		return a.usageError("prune", err)
	}
	ctx, stop := a.signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, _, err := a.reconciled(ctx, *server); err != nil {
		return a.fail("prune", err)
	}
	return ExitOK
}

// cmdStatus prints a one-line summary of the sessions a server's panes show,
// for the status line. tmux runs it every status-interval, so it also
// reconciles, like cmdPrune.
func (a *app) cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	styled := fs.Bool("tmux", false, "")
	server := fs.String("server", "", "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("status", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, w, err := a.reconciled(ctx, *server)
	if err != nil {
		return a.fail("status", err)
	}
	// The status line is drawn for an attached client, so a finished turn it
	// shows is one the person has seen. A failure leaves the count as it was;
	// the next tick tries again.
	if marked, err := query.MarkShown(ctx, a.log, v, w, *server); err == nil {
		for _, d := range marked {
			for i := range v.Sessions {
				if v.Sessions[i].Agent == d.Session.Agent && v.Sessions[i].ID == d.Session.ID {
					v.Sessions[i].Seen = d.Session.Seen
				}
			}
		}
	}
	fmt.Fprintln(a.stdout, Summary(v.Sessions, *styled))
	return ExitOK
}

// Summary counts sessions by what they need, in this order: blocked,
// finished (idle on a turn the person has not seen), working, and idle
// (settled), e.g. "1 blocked · 2 finished · 1 working · 3 idle". Blocked
// sessions are highlighted in tmux style when styled is set.
func Summary(sessions []query.Session, styled bool) string {
	count := map[string]int{}
	for _, s := range sessions {
		switch {
		case s.State == adapter.Blocked:
			count["blocked"]++
		case s.Finished():
			count["finished"]++
		case s.State == adapter.Working:
			count["working"]++
		case s.State == adapter.Idle:
			count["idle"]++
		}
	}
	var parts []string
	for _, state := range []string{"blocked", "finished", "working", "idle"} {
		if n := count[state]; n > 0 {
			part := fmt.Sprintf("%d %s", n, state)
			if styled && state == "blocked" {
				part = "#[fg=red,bold]" + part + "#[default]"
			}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " · ")
}

// cmdJump switches a client to the pane of the session that has waited
// longest for the user.
func (a *app) cmdJump(args []string) int {
	fs := flag.NewFlagSet("jump", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	client := fs.String("client", "", "")
	server := fs.String("server", "", "")
	from := fs.String("from", "", "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("jump", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	v, _, err := a.reconciled(ctx, *server)
	if err != nil {
		return a.fail("jump", err)
	}
	target, ok := Waiting(v, *from)
	if *client != "" {
		srv := tmux.DefaultServer()
		if *server != "" {
			srv = tmux.ServerByName(*server)
		}
		if !ok {
			_, err = srv.Run(ctx, "display-message", "-c", *client, "tmag: no agent is waiting for you")
		} else {
			id := target.ID
			_, err = srv.Run(ctx, "switch-client", "-c", *client, "-t", id, ";", "select-window", "-t", id, ";", "select-pane", "-t", id)
		}
		if err != nil {
			return a.fail("jump", err)
		}
		return ExitOK
	}
	if !ok {
		return a.fail("jump", fmt.Errorf("%w: no agent is waiting for you", query.ErrNotFound))
	}
	if a.json {
		a.emit(map[string]any{"pane": target})
	} else {
		fmt.Fprintf(a.stdout, "%s:%s\n", target.Server, target.ID)
	}
	return ExitOK
}

// Waiting picks the pane of the session that has waited longest for the
// user: blocked sessions first, then finished ones (idle on a turn the person
// has not seen), oldest first, skipping the pane the user is in. A settled
// session is never a target.
func Waiting(v query.View, fromPane string) (query.Pane, bool) {
	type candidate struct {
		pane    query.Pane
		blocked bool
		since   time.Time
	}
	var pool []candidate
	for _, p := range v.Panes {
		s, ok := v.Session(p.Agent, p.Session)
		if !ok || p.ID == fromPane {
			continue
		}
		blocked := s.State == adapter.Blocked
		if !blocked && !s.Finished() {
			continue
		}
		c := candidate{pane: p, blocked: blocked}
		if s.Since != nil {
			c.since = *s.Since
		}
		pool = append(pool, c)
	}
	if len(pool) == 0 {
		return query.Pane{}, false
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].blocked != pool[j].blocked {
			return pool[i].blocked
		}
		return pool[i].since.Before(pool[j].since)
	})
	return pool[0].pane, true
}
