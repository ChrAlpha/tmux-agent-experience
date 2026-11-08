package eventlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/contextio"
)

const (
	historyName    = "history.json"
	maxRecordBytes = 64 << 20
)

// ErrIntegrityLoss means a complete record or recovery checkpoint is invalid.
// Repairing an incomplete final write does not hide this kind of corruption.
var ErrIntegrityLoss = errors.New("event log integrity lost")

// IntegrityError identifies damage without including any record contents.
type IntegrityError struct {
	File   string
	Offset int64
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("%s: %s at byte %d", ErrIntegrityLoss, e.File, e.Offset)
}

func (e *IntegrityError) Unwrap() error { return ErrIntegrityLoss }

// history is a checkpoint of verified complete records, not a transaction over
// session files. Dirty is persisted before replacing or discarding a segment.
// Floor certifies that all committed records after it are still retained.
type history struct {
	Version    int     `json:"version"`
	Epoch      string  `json:"epoch"`
	Generation uint64  `json:"generation"`
	Origin     int64   `json:"origin"`
	Allocated  int64   `json:"allocated"`
	Committed  int64   `json:"committed"`
	Floor      int64   `json:"floor"`
	Dirty      bool    `json:"dirty,omitempty"`
	Rotated    segment `json:"rotated"`
	Current    segment `json:"current"`
}

type segment struct {
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	First    int64  `json:"first"`
	Last     int64  `json:"last"`
}

func segmentOf(st os.FileInfo) segment {
	s := segment{Size: st.Size(), Modified: st.ModTime().UnixNano()}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		s.Device, s.Inode = uint64(sys.Dev), sys.Ino //nolint:unconvert // Darwin Dev is int32; Linux uses uint64.
	}
	return s
}

func (s segment) sameFile(other segment) bool {
	return s.Inode != 0 && s.Device == other.Device && s.Inode == other.Inode
}

func (s segment) same(other segment) bool {
	return s.sameFile(other) && s.Size == other.Size && s.Modified == other.Modified
}

func newEpoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (l *Log) saveHistory(ctx context.Context, h history) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return l.publish(ctx, historyName, b)
}

// history verifies changes since the checkpoint and repairs unfinished final
// writes. The caller holds the log lock. Normal appends inspect only new bytes;
// a changed file identity or modified prefix requires a full bounded scan.
func (l *Log) history(ctx context.Context) (history, error) {
	var h history
	b, err := contextio.ReadFile(ctx, l.path(historyName), 64<<10)
	fresh := errors.Is(err, os.ErrNotExist)
	switch {
	case err != nil && !fresh:
		return h, err
	case !fresh:
		if json.Unmarshal(b, &h) != nil || h.Version != 1 || h.Epoch == "" ||
			h.Allocated < h.Committed || h.Floor > h.Allocated || h.Origin < 0 {
			return h, &IntegrityError{File: historyName}
		}
	default:
		h.Version = 1
		if h.Epoch, err = newEpoch(); err != nil {
			return h, err
		}
	}
	before := h
	// seq remains interoperable with older writers. A missing seq is harmless
	// because the independent checkpoint and verified records preserve its max.
	sequence := int64(0)
	if b, err := contextio.ReadFile(ctx, l.path(seqName), 1024); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			sequence = n
			h.Allocated = max(h.Allocated, n)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return h, err
	}
	var segments [2]segment
	var partial [2]bool
	rewritten := false
	lateReservation := false
	for i, name := range []string{rotatedName, currentName} {
		if err := ctx.Err(); err != nil {
			return h, err
		}
		st, err := os.Lstat(l.path(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return h, err
		}
		if !st.Mode().IsRegular() {
			return h, &IntegrityError{File: name}
		}
		actual := segmentOf(st)
		var known segment
		for _, candidate := range []segment{h.Rotated, h.Current} {
			if candidate.sameFile(actual) {
				known = candidate
				break
			}
		}
		if known.same(actual) {
			segments[i] = known
			continue
		}
		start := int64(0)
		previous := int64(0)
		if known.sameFile(actual) && actual.Size > known.Size {
			start = known.Size
			previous = known.Last
		} else if known.sameFile(actual) {
			// A same-inode truncation or prefix rewrite is not an append.
			// Even syntactically valid surviving records cannot certify it.
			rewritten = true
		}
		verified, _, unfinished, err := l.scan(ctx, name, start, previous, -1, false)
		if err != nil {
			return h, err
		}
		if !fresh && verified.First > 0 && verified.First <= before.Allocated {
			// A legacy writer can fill an already exposed reservation after
			// seq is removed or rolled back. A completed write whose final
			// checkpoint failed is indistinguishable, so invalidate conservatively.
			lateReservation = true
		}
		if start > 0 && known.First != 0 {
			verified.First = known.First
		}
		segments[i], partial[i] = verified, unfinished
		h.Allocated = max(h.Allocated, verified.Last)
	}
	rotated, current := segments[0], segments[1]
	if rotated.Last > 0 && current.First > 0 && rotated.Last >= current.First {
		return h, &IntegrityError{File: currentName}
	}
	h.Committed = max(rotated.Last, current.Last)
	if fresh {
		// An existing v1 log has no retention evidence. Its retained suffix
		// remains usable; do not infer that a leading allocation gap was lost.
		first := rotated.First
		if first == 0 {
			first = current.First
		}
		if first > 0 {
			h.Origin, h.Floor = first-1, first-1
		} else if h.Allocated > 0 {
			h.Origin, h.Floor = h.Allocated, h.Allocated
		}
	} else if before.Current.Inode != 0 || before.Rotated.Inode != 0 {
		switch {
		case before.Current.sameFile(current):
			// An older writer may have appended without checkpointing.
			if before.Rotated.Inode != 0 && !before.Rotated.sameFile(rotated) {
				h.Floor = max(h.Floor, before.Rotated.Last)
			}
		case before.Current.sameFile(rotated):
			// One legacy rotation: the former rotated file was discarded.
			h.Floor = max(h.Floor, before.Rotated.Last)
		case before.Current.Inode == 0 && before.Rotated.sameFile(rotated):
			// A new current file was not created after a planned rotation.
		default:
			// Unknown replacement/removal cannot establish what disappeared.
			// Fence off all previously issued numeric cursors as well as tokens.
			h.Generation++
			h.Allocated++
			h.Floor = h.Allocated
		}
	}
	h.Rotated, h.Current = rotated, current
	if rewritten || lateReservation {
		h.Generation++
		h.Allocated++
		h.Floor = h.Allocated
	}
	if partial[0] || partial[1] {
		// A follower may already hold the old inode and a partial buffer.
		// Reserve a fence above every prior cursor before replacing any file.
		h.Generation++
		h.Allocated++
		h.Floor, h.Dirty = h.Allocated, true
		if err := l.saveHistory(ctx, h); err != nil {
			return h, err
		}
		if err := l.writeSeq(ctx, h.Allocated); err != nil {
			return h, err
		}
		for i, name := range []string{rotatedName, currentName} {
			if !partial[i] {
				continue
			}
			fixed, err := l.replacePrefix(ctx, name, segments[i])
			if err != nil {
				return h, err
			}
			if i == 0 {
				h.Rotated = fixed
			} else {
				h.Current = fixed
			}
		}
	}
	h.Dirty = false
	if sequence < h.Allocated {
		if err := ctx.Err(); err != nil {
			return h, err
		}
		// Publish recovery fences to legacy writers before exposing a cursor.
		if err := l.writeSeq(ctx, h.Allocated); err != nil {
			return h, err
		}
	}
	if fresh || h != before {
		if err := l.saveHistory(ctx, h); err != nil {
			return h, err
		}
	}
	return h, nil
}

// scan reads complete records starting at a known record boundary. Size in its
// result is the last valid newline, so an unfinished tail can be replaced.
func (l *Log) scan(ctx context.Context, name string, offset, previous, after int64, collect bool) (segment, []Event, bool, error) {
	f, err := contextio.OpenRegular(ctx, l.path(name), os.O_RDONLY, 0)
	if err != nil {
		return segment{}, nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return segment{}, nil, false, err
	}
	s := segmentOf(st)
	s.Size, s.Last = offset, previous
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return s, nil, false, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var events []Event
	for {
		if err := ctx.Err(); err != nil {
			return s, nil, false, err
		}
		line, err := readRecord(ctx, r)
		if errors.Is(err, io.EOF) {
			return s, events, len(line) != 0, nil
		}
		if err != nil {
			return s, nil, false, err
		}
		var e Event
		if json.Unmarshal(line, &e) != nil || e.Seq <= s.Last {
			return s, nil, false, &IntegrityError{File: name, Offset: s.Size}
		}
		if s.First == 0 {
			s.First = e.Seq
		}
		s.Size += int64(len(line))
		s.Last = e.Seq
		if collect && e.Seq > after {
			events = append(events, e)
		}
	}
}

// readRecord checks cancellation between bounded reads, including for a damaged
// file without any newline. It never allocates in proportion to an entire log.
func readRecord(ctx context.Context, r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > maxRecordBytes {
			return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrIntegrityLoss, maxRecordBytes)
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func (l *Log) replacePrefix(ctx context.Context, name string, s segment) (segment, error) {
	in, err := contextio.OpenRegular(ctx, l.path(name), os.O_RDONLY, 0)
	if err != nil {
		return s, err
	}
	defer in.Close()
	tmp := l.path(name + ".repair.tmp")
	out, err := contextio.OpenRegular(ctx, tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return s, err
	}
	defer os.Remove(tmp)
	remaining := s.Size
	buf := make([]byte, 64<<10)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return s, err
		}
		n, err := io.ReadFull(in, buf[:min(int64(len(buf)), remaining)])
		if err != nil {
			_ = out.Close()
			return s, err
		}
		if err := contextio.WriteAll(ctx, out, buf[:n]); err != nil {
			_ = out.Close()
			return s, err
		}
		remaining -= int64(n)
	}
	if err := out.Close(); err != nil {
		return s, err
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if err := os.Rename(tmp, l.path(name)); err != nil {
		return s, err
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	st, err := os.Lstat(l.path(name))
	if err != nil {
		return s, err
	}
	if !st.Mode().IsRegular() {
		return s, &IntegrityError{File: name}
	}
	fixed := segmentOf(st)
	fixed.First, fixed.Last = s.First, s.Last
	return fixed, nil
}
