package codexidx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	lines := `{"id":"a","thread_name":"First","updated_at":"x"}
{"id":"b","thread_name":"Shared","updated_at":"x"}
{"id":"c","thread_name":"Shared","updated_at":"x"}
{"id":"a","thread_name":"Renamed","updated_at":"y"}
not json
`
	if err := os.WriteFile(filepath.Join(dir, "session_index.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if name := n.Name("a"); name != "Renamed" {
		t.Fatalf("a = %q", name)
	}
	if ids := n.Showing("Renamed"); len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("Renamed shown by %v", ids)
	}
	if ids := n.Showing("Shared"); len(ids) != 2 {
		t.Fatalf("Shared shown by %v", ids)
	}
	if ids := n.Showing("First"); len(ids) != 0 {
		t.Fatalf("a's old name still shown by %v", ids)
	}
	if name := n.Name("missing"); name != "" {
		t.Fatalf("missing = %q", name)
	}
	if _, err := Load(t.TempDir()); err != nil {
		t.Fatalf("missing index: %v", err)
	}
}

func TestDisplay(t *testing.T) {
	long := "Fix every integration test in the frontend workspace before release"
	for name, want := range map[string]string{
		"  Ship release \n": "Ship release",
		"":                  "",
		long:                "Fix every integration test in the frontend wo...",
		long[:48]:           long[:48], // exactly at the limit: shown whole
		long[:49]:           long[:45] + "...",
		// Clusters, not runes, count: 47 accented letters fit.
		strings.Repeat("e\u0301", 47): strings.Repeat("e\u0301", 47),
		strings.Repeat("🇯🇵", 49):      strings.Repeat("🇯🇵", 45) + "...",
		strings.Repeat("👩‍💻", 50):     strings.Repeat("👩‍💻", 45) + "...",
		strings.Repeat("修", 60):       strings.Repeat("修", 45) + "...",
	} {
		if got := Display(name); got != want {
			t.Errorf("Display(%q) = %q, want %q", name, got, want)
		}
	}
}
