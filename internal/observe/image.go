// Package observe coalesces metadata observation and bounded repair. Images
// never contain process tables, pane titles, callers or control authority.
package observe

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/statefile"
)

const MaxImageBytes = 16 << 20

var (
	ErrUnavailable = errors.New("observation_unavailable")
	ErrBusy        = errors.New("observation_busy")
	ErrCapacity    = errors.New("observation_capacity")
)

// Pane retains the v1 metadata-only pane view. It cannot be used as proof.
type Pane struct {
	Server   string `json:"server"`
	ID       string `json:"id"`
	Target   string `json:"target"`
	Cwd      string `json:"cwd"`
	Command  string `json:"command"`
	Agent    string `json:"agent"`
	Session  string `json:"session,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	View     string `json:"view,omitempty"`
}
type Image struct {
	Version    int                    `json:"version"`
	Generation uint64                 `json:"generation"`
	Created    time.Time              `json:"created"`
	Interval   int                    `json:"interval"`
	Start      eventlog.Checkpoint    `json:"start"`
	Records    []session.Record       `json:"records"`
	Panes      []Pane                 `json:"panes"`
	Report     attach.ReconcileReport `json:"report"`
}

func Panes(w *attach.World, records []session.Record) []Pane {
	out := []Pane{}
	for _, l := range attach.Links(w, records) {
		p := l.Client.Pane
		v := Pane{Server: p.Server, ID: p.ID, Target: p.Target(), Cwd: p.Path, Command: p.Command, Agent: l.Client.Agent}
		if l.Session != nil {
			v.Session, v.Evidence, v.View = l.Session.ID, l.Evidence, l.View
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Target < out[j].Target
	})
	return out
}
func readImage(ctx context.Context, root string) (Image, error) {
	return readImageUsing(ctx, root, statefile.ReadSmallWithin)
}
func readImageUsing(ctx context.Context, root string, read func(context.Context, string, string, int64) ([]byte, error)) (Image, error) {
	var img Image
	data, err := read(ctx, root, "observe/image.json", MaxImageBytes)
	if err != nil {
		return img, errors.Join(ErrUnavailable, err)
	}
	if json.Unmarshal(data, &img) != nil || img.Version != 1 || img.Generation == 0 || img.Created.IsZero() || img.Interval < 0 || img.Interval > 3600 {
		return Image{}, ErrUnavailable
	}
	if _, err := img.Start.Cursor(); err != nil {
		return Image{}, errors.Join(ErrUnavailable, err)
	}
	seen := map[[2]string]bool{}
	for _, r := range img.Records {
		key := [2]string{r.Agent, r.ID}
		if seen[key] || !session.ValidRecord(r) || r.Ended != "" {
			return Image{}, ErrUnavailable
		}
		seen[key] = true
	}
	for _, p := range img.Panes {
		if (p.Agent != "claude" && p.Agent != "codex") || p.Server == "" || p.ID == "" || (p.Session != "" && !seen[[2]string{p.Agent, p.Session}]) {
			return Image{}, ErrUnavailable
		}
	}
	return img, ctx.Err()
}

// ReadImage only admits bounded-age evidence. It never refreshes or proves a caller.
func ReadImage(ctx context.Context, root string) (Image, error) {
	img, err := readImage(ctx, root)
	if err != nil {
		return Image{}, err
	}
	age := time.Since(img.Created)
	ttl := time.Duration(max(2, img.Interval)*2) * time.Second
	if age < 0 || age > ttl {
		return Image{}, ErrUnavailable
	}
	return img, nil
}
