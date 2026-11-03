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
	"github.com/ChrAlpha/tmux-agent-experience/internal/bind"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// agentsOn lists agents, limited to one tmux server when server is set.
func agentsOn(ctx context.Context, server string) ([]query.Agent, error) {
	w, err := bind.Load(ctx)
	if err != nil {
		return nil, err
	}
	return onServer(query.Agents(w), server), nil
}

func onServer(all []query.Agent, server string) []query.Agent {
	if server == "" {
		return all
	}
	socket := tmux.ServerByName(server).Socket
	var out []query.Agent
	for _, a := range all {
		if a.Pane.Server == socket {
			out = append(out, a)
		}
	}
	return out
}

// cmdPrune clears records of agents that left their pane without a word, on
// one server or all. Pane borders and the picker read those records straight
// from tmux options, where nothing checks the agent process; the picker key
// runs this first. It prints nothing, so tmux shows nothing.
func (a *app) cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	server := fs.String("server", "", "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("prune", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w, err := bind.Load(ctx)
	if err != nil {
		return a.fail("prune", err)
	}
	if *server != "" {
		socket := tmux.ServerByName(*server).Socket
		w.Panes = slices.DeleteFunc(w.Panes, func(p tmux.Pane) bool { return p.Server != socket })
	}
	query.Prune(ctx, a.log.Dir, w, nil)
	return ExitOK
}

// cmdStatus prints a one-line summary for the status line. tmux runs it every
// status-interval, so it also prunes, like cmdPrune.
func (a *app) cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	styled := fs.Bool("tmux", false, "")
	server := fs.String("server", "", "")
	if _, err := parse(fs, args); err != nil {
		return a.usageError("status", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w, err := bind.Load(ctx)
	if err != nil {
		return a.fail("status", err)
	}
	fmt.Fprintln(a.stdout, Summary(onServer(query.Agents(w), *server), *styled))
	query.Prune(ctx, a.log.Dir, w, nil)
	return ExitOK
}

// Summary counts agents by state, e.g. "2 blocked · 1 working". Blocked agents
// are highlighted in tmux style when styled is set.
func Summary(agents []query.Agent, styled bool) string {
	count := map[string]int{}
	for _, a := range agents {
		count[a.State]++
	}
	var parts []string
	for _, state := range []string{adapter.Blocked, adapter.Working, adapter.Idle} {
		if n := count[state]; n > 0 {
			part := fmt.Sprintf("%d %s", n, state)
			if styled && state == adapter.Blocked {
				part = "#[fg=red,bold]" + part + "#[default]"
			}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " · ")
}

// cmdJump switches a client to the agent that has waited longest for the user.
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
	agents, err := agentsOn(ctx, *server)
	if err != nil {
		return a.fail("jump", err)
	}
	target, ok := Waiting(agents, *from)
	if *client != "" {
		srv := tmux.DefaultServer()
		if *server != "" {
			srv = tmux.ServerByName(*server)
		}
		if !ok {
			_, err = srv.Run(ctx, "display-message", "-c", *client, "tmag: no agent is waiting for you")
		} else {
			id := target.Pane.ID
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
		a.emit(map[string]any{"agent": target})
	} else {
		fmt.Fprintf(a.stdout, "%s:%s\n", target.Pane.Server, target.Pane.ID)
	}
	return ExitOK
}

// Waiting picks the agent that has waited longest for the user: blocked agents
// first, then idle ones, oldest first, skipping the pane the user is in.
func Waiting(agents []query.Agent, fromPane string) (query.Agent, bool) {
	var pool []query.Agent
	for _, a := range agents {
		if a.Binding != bind.Unbound && a.Pane.ID != fromPane && (a.State == adapter.Blocked || a.State == adapter.Idle) {
			pool = append(pool, a)
		}
	}
	if len(pool) == 0 {
		return query.Agent{}, false
	}
	rank := func(a query.Agent) int {
		if a.State == adapter.Blocked {
			return 0
		}
		return 1
	}
	since := func(a query.Agent) time.Time {
		if a.Since == nil {
			return time.Time{}
		}
		return *a.Since
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if rank(pool[i]) != rank(pool[j]) {
			return rank(pool[i]) < rank(pool[j])
		}
		return since(pool[i]).Before(since(pool[j]))
	})
	return pool[0], true
}
