package codexidx

import (
	"os"
	"path/filepath"
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
	if name, unique := n.Name("a"); name != "Renamed" || !unique {
		t.Fatalf("a = %q %v", name, unique)
	}
	if name, unique := n.Name("b"); name != "Shared" || unique {
		t.Fatalf("b = %q %v", name, unique)
	}
	if name, _ := n.Name("missing"); name != "" {
		t.Fatalf("missing = %q", name)
	}
	if _, err := Load(t.TempDir()); err != nil {
		t.Fatalf("missing index: %v", err)
	}
}
