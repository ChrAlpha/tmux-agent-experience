package ownedfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func publishedManifest(t *testing.T, root string) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".tmag-scope.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBootstrapPreservesUnknownManifestTemporary(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "directory", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, manifestTemporaryName(0))
			target := filepath.Join(root, "unrelated")
			if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "regular":
				err = os.WriteFile(path, []byte("unknown"), 0o600)
			case "symlink":
				err = os.Symlink("unrelated", path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "hardlink":
				err = os.Link(target, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			d, err := Open(context.Background(), root, "coord", true)
			if err != nil {
				t.Fatal(err)
			}
			_ = d.Close()
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("bootstrap removed collision: %v", err)
			}
			if kind == "regular" {
				if got, err := os.ReadFile(path); err != nil || string(got) != "unknown" {
					t.Fatalf("collision changed: %q %v", got, err)
				}
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "preserve" {
				t.Fatalf("alias target changed: %q %v", got, err)
			}
			if m := publishedManifest(t, root); m.Temporary != manifestTemporaryName(1) {
				t.Fatalf("reserved collision instead of free candidate: %q", m.Temporary)
			}
		})
	}
}

func interruptedManifest(t *testing.T, d *Dir, established string) string {
	t.Helper()
	gate, err := d.OpenFile(context.Background(), ".tmag-scope-lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if err := syscall.Flock(int(gate.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual allocation/reclamation boundary, then leave the
	// same partial bytes a killed publisher can leave before atomic Rename.
	f, name, err := d.manifestTemporary(context.Background(), established)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestFirstBootstrapInterruptionCanRetryWithoutClaimingOrphan(t *testing.T) {
	root, d := rootDir(t)
	orphan := interruptedManifest(t, d, "")
	if _, err := os.Lstat(filepath.Join(root, ".tmag-scope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture committed manifest: %v", err)
	}
	next, err := OpenSlots(context.Background(), root, "sessions/claude", ".slot-", 4, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Close()
	m := publishedManifest(t, root)
	if m.Temporary == orphan || m.Slots["sessions/claude/.slot-"] != 4 {
		t.Fatalf("bootstrap retry lost reservation: %+v", m)
	}
	if got, err := os.ReadFile(filepath.Join(root, orphan)); err != nil || string(got) != `{"version":` {
		t.Fatalf("unproven orphan changed: %q %v", got, err)
	}
}

func TestEstablishedInterruptedManifestPublicationRecovers(t *testing.T) {
	root := t.TempDir()
	first, err := OpenSlots(context.Background(), root, "sessions/claude", ".slot-", 4, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	before := publishedManifest(t, root)
	d, err := Open(context.Background(), root, ".", false)
	if err != nil {
		t.Fatal(err)
	}
	orphan := interruptedManifest(t, d, before.Temporary)
	_ = d.Close()
	// A new binding must publish while retaining the established directory
	// and slot evidence. The old checkpoint authorizes reclaiming this temp.
	next, err := Open(context.Background(), root, "coord", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Close()
	after := publishedManifest(t, root)
	if after.Temporary != before.Temporary || after.Dirs["sessions/claude"] != before.Dirs["sessions/claude"] || after.Slots["sessions/claude/.slot-"] != 4 || after.Dirs["coord"] == (identity{}) {
		t.Fatalf("recovery lost checkpoint evidence: %+v", after)
	}
	if _, err := os.Lstat(filepath.Join(root, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted temporary not reclaimed: %v", err)
	}
}

func TestLegacyManifestDoesNotAuthorizeUnknownTemporary(t *testing.T) {
	root := t.TempDir()
	d, err := Open(context.Background(), root, "coord", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	m := publishedManifest(t, root)
	m.Temporary = "" // The previous format has no temporary reservation.
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tmag-scope.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, manifestTemporaryName(0))
	if err := os.WriteFile(path, []byte("unproven old temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), root, "locks", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Close()
	if got, err := os.ReadFile(path); err != nil || string(got) != "unproven old temporary" {
		t.Fatalf("legacy collision changed: %q %v", got, err)
	}
	if m := publishedManifest(t, root); m.Temporary != manifestTemporaryName(1) {
		t.Fatalf("upgrade did not establish a free reservation: %+v", m)
	}
}

func TestEstablishedReservationStillPreservesAliasedTemporary(t *testing.T) {
	root := t.TempDir()
	d, err := Open(context.Background(), root, "coord", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	m := publishedManifest(t, root)
	marker := filepath.Join(root, "unrelated")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, m.Temporary)
	if err := os.Symlink("unrelated", alias); err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), root, "locks", true)
	if next != nil {
		_ = next.Close()
	}
	if !errors.Is(err, ErrScope) {
		t.Fatalf("reclaimed aliased reserved temporary: %v", err)
	}
	if got, err := os.Readlink(alias); err != nil || got != "unrelated" {
		t.Fatalf("alias changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserve" {
		t.Fatalf("alias target changed: %q %v", got, err)
	}
}

func TestBootstrapCandidateExhaustionPreservesEveryCollision(t *testing.T) {
	root := t.TempDir()
	for slot := range manifestTemporarySlots {
		if err := os.WriteFile(filepath.Join(root, manifestTemporaryName(slot)), []byte("unknown"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := Open(context.Background(), root, "coord", true)
	if d != nil {
		_ = d.Close()
	}
	if !errors.Is(err, ErrScope) {
		t.Fatalf("exhausted bootstrap = %v", err)
	}
	for slot := range manifestTemporarySlots {
		if got, err := os.ReadFile(filepath.Join(root, manifestTemporaryName(slot))); err != nil || string(got) != "unknown" {
			t.Fatalf("collision %d changed: %q %v", slot, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".tmag-scope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published without a free candidate: %v", err)
	}
}

func TestManifestReservationRequiresValidCheckpoint(t *testing.T) {
	for _, data := range []string{`{}`, `{"version":1,"directories":{}}`} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".tmag-scope.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := Open(context.Background(), root, "coord", true)
		if d != nil {
			_ = d.Close()
		}
		if !errors.Is(err, ErrScope) {
			t.Fatalf("incomplete checkpoint admitted: %s %v", data, err)
		}
	}
	root := t.TempDir()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	m := manifest{Version: 1, Root: id(info), Dirs: map[string]identity{}, Temporary: "unrelated"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tmag-scope.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "unrelated")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(context.Background(), root, "coord", true)
	if d != nil {
		_ = d.Close()
	}
	if !errors.Is(err, ErrScope) {
		t.Fatalf("invalid temporary reservation admitted: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserve" {
		t.Fatalf("invalid reservation removed unrelated file: %q %v", got, err)
	}
}
