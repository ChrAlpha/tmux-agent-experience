package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/ChrAlpha/tmux-agent-experience/internal/lockfile"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const HealthByteLimit = 1 << 20
const HealthRecordLimit = 4096

// Health reports only bounded retained-file evidence. It does not certify a
// replay checkpoint, repair a tail, adopt legacy metadata, or refresh an image.
type Health struct {
	State       string `json:"state"`
	Complete    bool   `json:"complete"`
	Bytes       int    `json:"bytes"`
	Records     int    `json:"complete_records"`
	ByteLimit   int    `json:"byte_limit"`
	RecordLimit int    `json:"record_limit"`
	LastSeq     int64  `json:"last_sequence,omitempty"`
	File        string `json:"file,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Note        string `json:"note"`
}

// ProbeHealth holds an existing log lock without waiting. Unknown ownership,
// capacity, contention and cancellation never become a healthy-log assertion.
func (l *Log) ProbeHealth(ctx context.Context) Health {
	h := Health{State: "unknown", ByteLimit: HealthByteLimit, RecordLimit: HealthRecordLimit, Note: "bounded retained records only; checkpoint continuity is not certified; no repair performed"}
	held, err := lockfile.TryExisting(ctx, l.Dir, lockName)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			h.State = "canceled"
		case errors.Is(err, lockfile.ErrBusy):
			h.State = "contended"
		case errors.Is(err, os.ErrNotExist):
			h.State = "never_recorded"
		default:
			h.State = "unreadable"
		}
		return h
	}
	defer held.Close()
	data, err := statefile.ReadKnown(ctx, l.Dir, historyName, 64<<10)
	if err == nil {
		h.Bytes += len(data)
		var checkpoint history
		if json.Unmarshal(data, &checkpoint) != nil || checkpoint.Version != 1 || checkpoint.Epoch == "" || checkpoint.Allocated < checkpoint.Committed || checkpoint.Floor > checkpoint.Allocated || checkpoint.Origin < 0 {
			h.State, h.File = "integrity_loss", historyName
			return h
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		h.State, h.File = "unreadable", historyName
		if ctx.Err() != nil {
			h.State = "canceled"
		}
		return h
	}
	files := 0
	for _, name := range []string{rotatedName, currentName} {
		data, err := statefile.ReadKnown(ctx, l.Dir, name, int64(HealthByteLimit-h.Bytes))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			h.State, h.File = "unreadable", name
			if errors.Is(err, statefile.ErrReadCapacity) {
				h.State = "capacity"
			}
			if ctx.Err() != nil {
				h.State = "canceled"
			}
			return h
		}
		files++
		h.Bytes += len(data)
		offset := 0
		for len(data) > 0 {
			if ctx.Err() != nil {
				h.State = "canceled"
				return h
			}
			if h.Records == HealthRecordLimit {
				h.State, h.File, h.Offset = "capacity", name, int64(offset)
				return h
			}
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				h.State, h.File, h.Offset = "incomplete_tail", name, int64(offset)
				return h
			}
			var e Event
			if json.Unmarshal(data[:end], &e) != nil || e.Seq <= h.LastSeq {
				h.State, h.File, h.Offset = "integrity_loss", name, int64(offset)
				return h
			}
			h.LastSeq = e.Seq
			h.Records++
			offset += end + 1
			data = data[end+1:]
		}
	}
	if ctx.Err() != nil {
		h.State = "canceled"
		return h
	}
	h.State, h.Complete = "readable", true
	if files == 0 {
		h.State = "never_recorded"
	}
	return h
}
