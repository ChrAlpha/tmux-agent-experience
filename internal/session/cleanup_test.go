package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/coord"
	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

func setTempSweep(t *testing.T, store Store, slot byte) {
	t.Helper()
	err := lockfile.Within(context.Background(), store.Dir, "coord/sweep-lock", false, true, func() error {
		path := "coord/sweep"
		return statefile.WriteWithin(context.Background(), store.Dir, path, path+".tmp", []byte(fmt.Sprintf("%d\n", slot)))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCleanupSkipsActiveCreatorAndRecoversAfterForcedExit(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := Record{Agent: "claude", ID: "crashed", Main: "idle", Since: t0}
	if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.PutContext(ctx, rec) }); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestTempPublisherHelper$")
	cmd.Env = append(os.Environ(), "TMAG_TEST_PUBLISHER_DIR="+store.Dir)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(pipe).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("publisher did not reach crash boundary: %q, %v", line, err)
	}
	slot := coord.Slot(sessionKey(rec.Agent, rec.ID))
	tmp := store.temporary(rec.Agent, slot)
	other := store.temporary("codex", slot+1)
	claimTempSlots(t, store, "codex")
	if err := os.WriteFile(other, []byte("unfinished metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	setTempSweep(t, store, slot)
	report, err := store.CleanupTemps(ctx)
	if err != nil || report.Slots > tempSweepSlots || report.Examined > 2*tempSweepSlots {
		t.Fatalf("cleanup budget = %+v, %v", report, err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("removed active publisher's temporary: %v", err)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy stripe starved next candidate: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	setTempSweep(t, store, slot)
	if _, err := store.CleanupTemps(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced-exit orphan survived its excluded sweep: %v", err)
	}
	if got, ok, err := store.Get(rec.Agent, rec.ID); err != nil || !ok || got.Main != "idle" {
		t.Fatalf("temporary cleanup changed published state: %+v, %v, %v", got, ok, err)
	}
}

// Simulate death after a partial metadata write while holding the actual
// production session coordination. Parent owns and kills only this subprocess.
func TestTempPublisherHelper(t *testing.T) {
	dir := os.Getenv("TMAG_TEST_PUBLISHER_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	store := Store{Dir: dir}
	err := store.Lock(context.Background(), "claude", "crashed", func() error {
		tmp := store.temporary("claude", coord.Slot(sessionKey("claude", "crashed")))
		if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(tmp, []byte(`{"agent":`), 0o600); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
			return err
		}
		<-time.After(time.Hour)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixedTempNamesBoundCrashChurnAndSkipLegacyFiles(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	ctx := context.Background()
	if err := coord.RetireLegacy(ctx, store.Dir); err != nil {
		t.Fatal(err)
	}
	claimTempSlots(t, store, "claude")
	claimTempSlots(t, store, "codex")
	// Every interrupted publisher can leave only its agent/stripe pathname.
	for i := range 2048 {
		id := "session-" + strconv.Itoa(i)
		for _, agent := range []string{"claude", "codex"} {
			err := store.Lock(ctx, agent, id, func() error {
				path := store.temporary(agent, coord.Slot(sessionKey(agent, id)))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					return err
				}
				return os.WriteFile(path, []byte(`{"agent":`), 0o600)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	count := 0
	for _, agent := range []string{"claude", "codex"} {
		entries, err := os.ReadDir(filepath.Dir(store.temporary(agent, 0)))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmag-tmp-s-") {
				count++
			}
		}
	}
	if count > 2*coord.Slots {
		t.Fatalf("unbounded fixed temporaries: %d", count)
	}
	legacy := filepath.Join(store.root(), "claude", ".tmp-legacy")
	if err := os.WriteFile(legacy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 * coord.Slots {
		report, err := store.CleanupTemps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if report.Slots > tempSweepSlots || report.Examined > 2*tempSweepSlots {
			t.Fatalf("exceeded per-pass budget: %+v", report)
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy temporary lost without creator proof: %v", err)
	}
	for _, agent := range []string{"claude", "codex"} {
		entries, err := os.ReadDir(filepath.Dir(store.temporary(agent, 0)))
		remaining := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmag-tmp-s-") {
				remaining++
			}
		}
		if err != nil || remaining != 0 {
			t.Fatalf("fixed orphan sweep failed to progress for %s: %d files, %v", agent, remaining, err)
		}
	}
}

func claimTempSlots(t *testing.T, store Store, agent string) {
	t.Helper()
	d, err := ownedfs.OpenSlots(context.Background(), store.Dir, filepath.Join("sessions", agent), ".tmag-tmp-s-", coord.Slots, true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

func TestTempMaintenanceAndPublicationPreserveAliasedAncestors(t *testing.T) {
	for _, level := range []string{"sessions", "sessions/claude", "sessions/claude/.tmag-tmp"} {
		t.Run(level, func(t *testing.T) {
			store := Store{Dir: t.TempDir()}
			foreign := t.TempDir()
			ctx := context.Background()
			path := filepath.Join(store.Dir, level)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(foreign, path); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"s-00", ".tmag-tmp-s-00", "record.json"} {
				if err := os.WriteFile(filepath.Join(foreign, name), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			setTempSweep(t, store, 0)
			if _, err := store.CleanupTemps(ctx); err != nil {
				t.Fatal(err)
			}
			rec := Record{Agent: "claude", ID: "record", Main: "idle", Since: t0}
			err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.PutContext(ctx, rec) })
			if level != "sessions/claude/.tmag-tmp" && err == nil {
				t.Fatal("published through an aliased parent")
			}
			// The retired draft's .tmag-tmp directory is never traversed, even
			// when its final file looks like one of the old fixed positions.
			for _, name := range []string{"s-00", ".tmag-tmp-s-00", "record.json"} {
				if b, err := os.ReadFile(filepath.Join(foreign, name)); err != nil || string(b) != "preserve" {
					t.Fatalf("aliased target changed %s: %q %v", name, b, err)
				}
			}
		})
	}
}

func TestTempCleanupRejectsRealDirectoryReplacementAndUnclaimedSlots(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(strconv.FormatBool(claim), func(t *testing.T) {
			store := Store{Dir: t.TempDir()}
			ctx := context.Background()
			path := filepath.Join(store.Dir, "sessions", "claude")
			if claim {
				claimTempSlots(t, store, "claude")
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			slot := store.temporary("claude", 0)
			if err := os.WriteFile(slot, []byte("unknown"), 0600); err != nil {
				t.Fatal(err)
			}
			setTempSweep(t, store, 0)
			if _, err := store.CleanupTemps(ctx); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(slot); err != nil || string(b) != "unknown" {
				t.Fatalf("uncertain slot removed: %q %v", b, err)
			}
			rec := Record{Agent: "claude", ID: "record", Main: "idle", Since: t0}
			if err := store.Lock(ctx, rec.Agent, rec.ID, func() error { return store.PutContext(ctx, rec) }); err == nil {
				t.Fatal("publication adopted uncertain namespace")
			}
		})
	}
}
