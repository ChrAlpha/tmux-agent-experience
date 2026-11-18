package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// KeyNames are the keys tmag keys presses, by tmux's names: what a person
// answers a dialog of an agent's with.
var KeyNames = []string{
	"Enter", "Escape", "Tab", "BTab", "Up", "Down", "Left", "Right", "Space", "BSpace", "C-c",
	"y", "n", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
}

// maxKeys and maxKeysText bound what one keys command types.
const (
	maxKeys     = 32
	maxKeysText = 4096
)

// ErrKeys reports keys tmag does not press, or too many of them.
var ErrKeys = errors.New("keys")

// Screen names a shot of a pane for the person to answer: how many lines it
// took and their digest (see tmux.Shot), as read --lines --json gives it.
func Screen(lines int, shot tmux.Shot) string { return strconv.Itoa(lines) + ":" + shot.Digest }

// maxScreenLines bounds the lines a screen names, as read --lines does.
const maxScreenLines = 2000

// screenLines is how many lines the screen token names, or 0 for no token
// a read gave.
func screenLines(screen string) int {
	n, digest, ok := strings.Cut(screen, ":")
	lines, err := strconv.Atoi(n)
	if !ok || err != nil || lines < 1 || lines > maxScreenLines || len(digest) != 64 {
		return 0
	}
	return lines
}

// Keys types into the pane target names, or that shows the session it
// names, for the person: text as a bracketed paste, then keys. It types only
// after checking, under the pane's lock and right before typing, that the
// pane shows the screen the person read, by a shot of as many lines (see
// read --lines --json); text and keys then go in one tmux command. tmux has
// no compare-and-type: what the agent draws, or what is typed into the pane
// by other means, between that check and the typing still comes first. The
// Keys event, logged before the lock is released, records the pane and how
// much was typed, never what. It is the person's own input, not a prompt
// tmag delivers: no agent may use it, and it is the way to answer what tmag
// does not model, such as an agent's trust dialog.
func Keys(ctx context.Context, log *eventlog.Log, target, screen, text string, keys []string) (Delivery, error) {
	text = tmux.SanitizePrompt(text)
	lines := screenLines(screen)
	switch {
	case lines == 0:
		return Delivery{}, fmt.Errorf("%w: --screen takes the screen a read with --lines --json gave", ErrKeys)
	case len(keys) > maxKeys || len(text) > maxKeysText:
		return Delivery{}, fmt.Errorf("%w: at most %d keys and %d bytes of text", ErrKeys, maxKeys, maxKeysText)
	case len(keys) == 0 && text == "":
		return Delivery{}, fmt.Errorf("%w: nothing to type", ErrKeys)
	}
	for _, k := range keys {
		if !slices.Contains(KeyNames, k) {
			return Delivery{}, fmt.Errorf("%w: %q is none of %v", ErrKeys, k, KeyNames)
		}
	}
	v, caller, err := prepare(ctx, log)
	if err != nil {
		return Delivery{}, err
	}
	if caller.By != nil {
		return Delivery{}, &RefusedError{RefuseAgent, "keys are the person's: an agent talks to another through tmag's verbs"}
	}
	t, err := Resolve(ctx, v, target)
	if err != nil {
		return Delivery{}, err
	}
	var p Pane
	switch {
	case t.Pane != nil:
		p = *t.Pane
	default:
		if p, err = paneOf(*t.Session, nil, v); err != nil {
			return Delivery{}, err
		}
	}
	srv := tmux.Server{Socket: p.Server}
	d := Delivery{Pane: &p, Via: "pane"}
	if t.Session != nil {
		d.Session = *t.Session
	}
	// The text is loaded first, so that only one tmux command follows the
	// check; a buffer not pasted is deleted.
	buf := ""
	if text != "" {
		if buf, err = srv.Load(ctx, text); err != nil {
			return Delivery{}, err
		}
	}
	pasted := false
	defer func() {
		if buf != "" && !pasted {
			srv.Discard(ctx, buf)
		}
	}()
	err = attach.Lock(ctx, log.Dir, p.Server, p.ID, func() error {
		shot, err := srv.Shot(ctx, p.ID, lines)
		if err != nil {
			return err
		}
		if Screen(lines, shot) != screen {
			return &RefusedError{RefuseChanged, fmt.Sprintf("%s shows another screen than the one read", p.ID)}
		}
		ev := eventlog.Event{Event: eventlog.Keys, Agent: p.Agent, Session: p.Session, Cwd: p.Cwd,
			Pane: &eventlog.PaneRef{Server: p.Server, ID: p.ID}, Keys: len(keys), Bytes: len(text)}
		caller.stamp(&ev)
		err = srv.Type(ctx, p.ID, buf, keys)
		pasted = err == nil
		if err != nil {
			return fmt.Errorf("typing into %s failed, perhaps after some keys reached it: %w", p.ID, err)
		}
		if err := log.Append(ctx, &ev); err != nil {
			return fmt.Errorf("typed into %s but not logged; do not repeat it: %w", p.ID, err)
		}
		d.Seq = ev.Seq
		return nil
	})
	return d, err
}
