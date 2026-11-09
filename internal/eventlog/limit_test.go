package eventlog

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// files is every file of the log directory with its contents.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		out[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unchanged fails unless the log's files and head are what they were.
func unchanged(t *testing.T, l *Log, before map[string]string, head Cursor) {
	t.Helper()
	after := files(t, l.Dir)
	if len(after) != len(before) {
		t.Fatalf("files changed: %d before, %d after", len(before), len(after))
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("%s changed", filepath.Base(name))
		}
	}
	if now, err := l.Head(bg); err != nil || now != head {
		t.Fatalf("head moved: %+v -> %+v (%v)", head, now, err)
	}
}

func refused(t *testing.T, l *Log, e Event) {
	t.Helper()
	head, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	before := files(t, l.Dir)
	err = l.Append(bg, &e)
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Limit != MaxRecordBytes || limit.Size <= MaxRecordBytes {
		t.Fatalf("Append = %v, want a limit refusal", err)
	}
	if e.Seq != 0 {
		t.Fatalf("a refused event was numbered %d", e.Seq)
	}
	unchanged(t, l, before, head)
}

// padded returns e with its cwd grown until Admit counts exactly size bytes.
func padded(t *testing.T, e Event, size int) Event {
	t.Helper()
	e.Cwd = "x"
	n, err := Admit(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Cwd = strings.Repeat("x", 1+size-n)
	return e
}

func TestAppendAdmitsALineOfExactlyTheLimit(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	e := padded(t, Event{Agent: "claude", Session: "s", Event: "Send"}, MaxRecordBytes)
	ok := e
	if err := l.Append(bg, &ok); err != nil {
		t.Fatalf("a line of exactly the limit: %v", err)
	}
	// Exercise the actual line reader without the public capture's wall-clock
	// budget: race instrumentation of a 64 MiB boundary is not a latency test.
	_, all, partial, err := l.scan(bg, currentName, 0, 0, 0, true)
	if err != nil || partial || len(all) != 2 || all[1].Seq != ok.Seq || len(all[1].Cwd) != len(e.Cwd) {
		t.Fatalf("read back %d events, %v", len(all), err)
	}
	data, err := os.ReadFile(l.path(currentName))
	if err != nil {
		t.Fatal(err)
	}
	if line := data[bytes.LastIndexByte(data[:len(data)-1], '\n')+1:]; len(line) > MaxRecordBytes {
		t.Fatalf("written line is %d bytes", len(line))
	}

	e.Cwd += "x"
	refused(t, l, e)
}

// Encoding, not the length of the strings given, is what counts: HTML
// characters and control bytes encode to six bytes each, and an invalid UTF-8
// byte to at least the three of a replacement character (six where the
// toolchain escapes it).
func TestAppendCountsEncodedBytes(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	for c, least := range map[string]int{"<": 6, "&": 6, "\x01": 6, "\x80": 3} {
		e := Event{Agent: "claude", Session: "s", Event: "Send", Cwd: strings.Repeat(c, MaxRecordBytes/least+1)}
		if len(e.Cwd) >= MaxRecordBytes/2 {
			t.Fatal("the raw string is not well under the limit")
		}
		refused(t, l, e)
	}
}

// A time the event has counts as it encodes, offset included.
func TestAppendCountsTheEventsOwnTime(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	at := time.Date(2026, 11, 9, 4, 40, 10, 0, time.UTC)
	e := padded(t, Event{Agent: "claude", Session: "s", Event: "Send", Time: at}, MaxRecordBytes)
	if n, err := Admit(e); err != nil || n != MaxRecordBytes {
		t.Fatalf("Admit = %d, %v", n, err)
	}
	e.Time = at.In(time.FixedZone("", 5*3600+1800))
	refused(t, l, e)
}

func TestAppendRefusesExhaustedSequence(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	if err := l.writeSeq(bg, math.MaxInt64-1); err != nil {
		t.Fatal(err)
	}
	last := Event{Agent: "claude", Session: "s", Event: "Stop"}
	if err := l.Append(bg, &last); err != nil || last.Seq != math.MaxInt64 {
		t.Fatalf("last number: %d, %v", last.Seq, err)
	}
	head, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	before := files(t, l.Dir)
	next := Event{Agent: "claude", Session: "s", Event: "Stop"}
	if err := l.Append(bg, &next); !errors.Is(err, ErrIntegrityLoss) || next.Seq != 0 {
		t.Fatalf("Append past the last number = %d, %v", next.Seq, err)
	}
	unchanged(t, l, before, head)
	if all, _, err := l.Since(bg, 0); err != nil || len(all) != 2 || all[1].Seq != math.MaxInt64 {
		t.Fatalf("events after exhaustion: %d, %v", len(all), err)
	}
}

// A canceled append returns at once, before encoding even an event too large
// to log.
func TestCanceledAppendReturnsBeforeEncoding(t *testing.T) {
	l := Open(t.TempDir())
	appendN(t, l, 1)
	head, err := l.Head(bg)
	if err != nil {
		t.Fatal(err)
	}
	before := files(t, l.Dir)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	e := Event{Agent: "claude", Session: "s", Event: "Send", Cwd: strings.Repeat("<", MaxRecordBytes/6+1)}
	if err := l.Append(ctx, &e); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append = %v", err)
	}
	unchanged(t, l, before, head)
}
