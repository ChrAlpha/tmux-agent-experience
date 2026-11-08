package eventlog

import (
	"encoding/hex"
	"errors"
)

// Checkpoint is the internal, versioned persisted form of a full cursor. It is
// not a v1 numeric client cursor. Segment position only accelerates replay;
// epoch and generation independently establish log continuity.
type Checkpoint struct {
	Version    int     `json:"version"`
	Seq        int64   `json:"seq"`
	Epoch      string  `json:"epoch"`
	Generation uint64  `json:"generation"`
	Current    segment `json:"current"`
}

func (c Cursor) Checkpoint() (Checkpoint, error) {
	p := Checkpoint{Version: 1, Seq: c.Seq, Epoch: c.epoch, Generation: c.generation, Current: c.current}
	_, err := p.Cursor()
	return p, err
}
func (p Checkpoint) Cursor() (Cursor, error) {
	b, err := hex.DecodeString(p.Epoch)
	s := p.Current
	if err != nil || len(b) != 16 || p.Version != 1 || p.Seq < 0 || s.Size < 0 || s.First < 0 || s.Last < s.First || s.Last > p.Seq || (s.Size > 0 && s.Inode == 0) {
		return Cursor{}, errors.New("observation_unavailable: invalid replay checkpoint")
	}
	return Cursor{Seq: p.Seq, epoch: p.Epoch, generation: p.Generation, current: p.Current}, nil
}
