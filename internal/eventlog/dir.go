package eventlog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
)

// DefaultDir resolves the state directory: $TMAG_STATE_DIR as it is, or
// else the directory of this process's realm (proc.Realm) under Base. Each
// realm has its own because tmag proves sessions from the process table it
// sees: two containers sharing a home would otherwise end each other's
// sessions, whose processes neither can see.
func DefaultDir() string {
	if d := os.Getenv("TMAG_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(Base(), proc.Realm())
}

// Base is where the realms' state directories are: $XDG_STATE_HOME/tmag,
// else ~/.local/state/tmag.
func Base() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tmag")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "state", "tmag")
}

// StaleAge is how long state under Base that no tmag of this realm uses goes
// unwritten before PruneStale removes it. Nothing tells a realm that ended,
// a container gone or a boot past, from one idle elsewhere, so only age does.
const StaleAge = 30 * 24 * time.Hour

var realmName = regexp.MustCompile(`^[0-9a-f]{12}$`)

// legacy are the names tmag 0.2.0 and earlier kept directly in Base, for
// every realm at once.
var legacy = []string{currentName, rotatedName, seqName, lockName, "locks", "sessions", "req", "hook.log"}

// PruneStale removes from Base the state no tmag of dir's realm uses and
// that was last written before cutoff: other realms' directories, and the
// state tmag 0.2.0 and earlier kept directly in Base. Only a realm's own
// directory prunes; any other dir, as $TMAG_STATE_DIR names, leaves Base
// alone. It returns what it removed.
func PruneStale(dir string, cutoff time.Time) ([]string, error) {
	dir = filepath.Clean(dir)
	base := filepath.Dir(dir)
	if base != filepath.Clean(Base()) || !realmName.MatchString(filepath.Base(dir)) {
		return nil, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var groups [][]string
	var old []string
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		switch {
		case p == dir:
		case e.IsDir() && realmName.MatchString(e.Name()):
			groups = append(groups, []string{p})
		case slices.Contains(legacy, e.Name()):
			old = append(old, p)
		}
	}
	if len(old) > 0 {
		groups = append(groups, old) // one state: removed whole or kept whole
	}
	var removed []string
	var errs []error
	for _, g := range groups {
		if newest(g).After(cutoff) {
			continue
		}
		for _, p := range g {
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, p)
		}
	}
	return removed, errors.Join(errs...)
}

// newest is the latest modification time of the files under paths. Every
// change tmag makes appends an event, so a state's files say when it was last
// used; directories change only when files come and go.
func newest(paths []string) time.Time {
	var t time.Time
	for _, root := range paths {
		_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() { // what it cannot read dates nothing
				if info, err := d.Info(); err == nil && info.ModTime().After(t) {
					t = info.ModTime()
				}
			}
			return nil
		})
	}
	return t
}
