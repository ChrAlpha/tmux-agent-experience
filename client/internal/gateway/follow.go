package gateway

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/client/internal/machine"
)

// Retry bounds: a broken watch is tried again after a second, then twice as
// long each time up to half a minute; a watch that ran a minute starts over.
const (
	retryFirst = time.Second
	retryMost  = 30 * time.Second
	retryReset = time.Minute
)

// follow watches one machine for as long as ctx lasts, resuming each new
// watch after the last event it applied: tmag replays what was missed, or
// sends a new snapshot when it cannot.
func (g *Gateway) follow(ctx context.Context, m machine.Machine) {
	wait := retryFirst
	for ctx.Err() == nil {
		began := time.Now()
		err := m.Watch(ctx, g.world.cursor(m.Name), func(d machine.Doc) error {
			if e := g.world.apply(m.Name, d); e != nil {
				g.alert(m.Name, e)
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		g.world.fail(m.Name, err)
		g.cfg.Log.Printf("%s: watch ended: %v", m.Name, err)
		if time.Since(began) > retryReset {
			wait = retryFirst
		}
		jitter := time.Duration(rand.Int64N(int64(wait / 4)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait + jitter):
		}
		wait = min(2*wait, retryMost)
	}
}

// watchDevices ends the streams of devices that are no longer paired, also
// when `tmag-client auth revoke` unpaired them from another process, or
// their credential expired.
func (g *Gateway) watchDevices(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, s := range g.world.streamsOf("") {
				s.close()
			}
			return
		case <-tick.C:
		}
		for _, s := range g.world.streamsOf("") {
			if _, ok, err := g.cfg.Devices.Device(s.device); err == nil && !ok {
				s.close()
			}
		}
	}
}
