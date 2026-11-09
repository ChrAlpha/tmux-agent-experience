package answer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/ChrAlpha/tmux-agent-experience/internal/ownedfs"
	"github.com/ChrAlpha/tmux-agent-experience/internal/socketbind"
)

// Presence never treats a directory entry as proof of a live listener. Unknown
// includes timeout, unsupported protocol, permission and identity errors;
// only definite absence/refusal authorizes stale cleanup.
type Presence uint8

const (
	Unknown Presence = iota
	Alive
	Dead
)

// exchangeBudget bounds an exchange that is not the operation itself: a
// probe, or telling a hook its prompt closed. Its connection starts a
// socketbind helper, a process start that a loaded machine stretches far past
// the exchange; a shorter budget reports a live or departed hook as Unknown
// and leaves the hook of a closed prompt waiting. The helper gives up on its
// own after socketbind.Timeout, so waiting longer gains nothing.
const exchangeBudget = socketbind.Timeout

func Probe(ctx context.Context, root, id string) Presence {
	ctx, cancel := context.WithTimeout(ctx, exchangeBudget)
	defer cancel()
	scope, name, _, err := lookup(ctx, root, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Dead
		}
		return Unknown
	}
	defer scope.Close()
	return probeAt(ctx, scope, name, id)
}

// Waiting is the compatibility convenience helper, scoped to a bounded probe.
// Production context-bearing callers use Probe; Unknown still permits an
// identity-checked operation through the existing request/state gates.
func Waiting(root, id string) bool { return Probe(context.Background(), root, id) == Alive }

func probeAt(ctx context.Context, scope *ownedfs.Dir, name, id string) Presence {
	ctx, cancel := context.WithTimeout(ctx, exchangeBudget)
	defer cancel()
	c, err := dialAt(ctx, scope, name)
	if err != nil {
		if ctx.Err() == nil && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)) {
			return Dead
		}
		return Unknown
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if err := json.NewEncoder(c).Encode(message{Op: "probe", Request: id}); err != nil {
		return Unknown
	}
	var r struct {
		OK      bool   `json:"ok"`
		Request string `json:"request"`
	}
	decoder := json.NewDecoder(io.LimitReader(c, 512))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil || !r.OK || r.Request != id || ctx.Err() != nil {
		return Unknown
	}
	return Alive
}
