// Package codexidx reads Codex's thread index to resolve thread ids to names
// and names, as the Codex TUI shows them, back to threads.
package codexidx

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Home returns $CODEX_HOME or ~/.codex.
func Home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

type entry struct {
	ID   string `json:"id"`
	Name string `json:"thread_name"`
}

// Names maps thread ids to their latest names, and the form Codex shows a
// name in back to the threads that show it.
type Names struct {
	byID    map[string]string
	byShown map[string][]string
}

// Empty is an index without threads.
func Empty() *Names {
	return &Names{byID: map[string]string{}, byShown: map[string][]string{}}
}

// Load reads session_index.jsonl under codexHome. A missing file yields an empty index.
func Load(codexHome string) (*Names, error) {
	n := Empty()
	f, err := os.Open(filepath.Join(codexHome, "session_index.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		n.byID[e.ID] = e.Name // later lines win: renames append new entries
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for id, name := range n.byID {
		if shown := Display(name); shown != "" {
			n.byShown[shown] = append(n.byShown[shown], id)
		}
	}
	return n, nil
}

// Name returns the thread's name, or "" when it has none.
func (n *Names) Name(id string) string { return n.byID[id] }

// Showing returns the threads whose name Codex displays as shown. Several
// threads may display alike, a long name by its truncated head.
func (n *Names) Showing(shown string) []string { return n.byShown[shown] }
