package claudestatus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "1.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadKeepsOnlyTheStatusFields(t *testing.T) {
	path := write(t, `{"sessionId":"s1","status":"idle","statusUpdatedAt":1700000000123,"procStart":"42","model":"secret"}`)
	s, err := Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "s1" || s.Status != "idle" || s.StatusUpdatedAt != 1700000000123 || s.ProcStart != "42" {
		t.Fatalf("status = %+v", s)
	}
}

func TestReadRejectsWhatIsNotAStatusFile(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":     `{`,
		"not an object": `[]`,
		"too large":     `{"sessionId":"` + strings.Repeat("s", MaxBytes) + `"}`,
		"empty":         ``,
	} {
		if _, err := Read(context.Background(), write(t, body)); err == nil {
			t.Errorf("%s: Read succeeded", name)
		}
	}
	if _, err := Read(context.Background(), filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file was read")
	}
}

func TestIdleAfterProvesTheSessionAndProcess(t *testing.T) {
	last := time.Unix(1700000000, 0)
	now := last.Add(140 * time.Millisecond)
	s := Status{SessionID: "s1", Status: "idle", StatusUpdatedAt: now.UnixMilli(), ProcStart: "42"}
	if !s.IdleAfter("s1", "42", last) {
		t.Fatal("a fresh idle status did not prove the turn over")
	}
	for name, c := range map[string]struct {
		s       Status
		session string
		start   string
		last    time.Time
	}{
		"another session": {s, "s2", "42", last},
		"another process": {Status{SessionID: "s1", Status: "idle", StatusUpdatedAt: now.UnixMilli(), ProcStart: "43"}, "s1", "42", last},
		"busy":            {Status{SessionID: "s1", Status: "busy", StatusUpdatedAt: now.UnixMilli(), ProcStart: "42"}, "s1", "42", last},
		"waiting":         {Status{SessionID: "s1", Status: "waiting", StatusUpdatedAt: now.UnixMilli(), ProcStart: "42"}, "s1", "42", last},
		"not later":       {Status{SessionID: "s1", Status: "idle", StatusUpdatedAt: last.UnixMilli(), ProcStart: "42"}, "s1", "42", last},
		"before":          {Status{SessionID: "s1", Status: "idle", StatusUpdatedAt: last.Add(-time.Second).UnixMilli(), ProcStart: "42"}, "s1", "42", last},
		"no time":         {Status{SessionID: "s1", Status: "idle", ProcStart: "42"}, "s1", "42", last},
	} {
		if c.s.IdleAfter(c.session, c.start, c.last) {
			t.Errorf("%s: proved idle", name)
		}
	}
}

// A status path that names a FIFO, directly or through a symlink, must not
// hold a reader even when no writer ever opens it: the file is opened
// non-blocking and rejected as nonregular before any read.
func TestReadDoesNotBlockOnANonregularFile(t *testing.T) {
	for name, throughLink := range map[string]bool{"fifo": false, "symlink": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "status.fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			path := fifo
			if throughLink {
				path = filepath.Join(dir, "status.json")
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := Read(ctx, path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a nonregular status file was read")
				}
			case <-time.After(250 * time.Millisecond):
				// Release an open that a regression left blocked, then fail.
				if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					w.Close()
				}
				select {
				case err := <-done:
					_ = err
				case <-time.After(time.Second):
					t.Error("the reader did not return after the FIFO was released")
				}
				t.Fatalf("Read blocked on a nonregular status file for %s", time.Since(started))
			}
		})
	}
}
