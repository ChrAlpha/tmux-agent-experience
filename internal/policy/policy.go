// Package policy resolves bounded realm-wide options without treating lookup
// uncertainty as permission. It never writes user configuration.
package policy

import (
	"context"
	"strconv"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

const Timeout = 100 * time.Millisecond
const MaxServers = 16

type Spec struct {
	Name             string
	Default, Maximum int
	Longest          bool
}

var (
	Watch       = Spec{"@tmag-watch-limit", 32, 256, false}
	Notify      = Spec{"@tmag-notify-limit", 4, 256, false}
	Reconcile   = Spec{"@tmag-reconcile-interval", 2, 3600, true}
	Observation = Spec{"@tmag-observation-age", 900, 86400, true}
	Diagnostics = Spec{"@tmag-diagnostics", 1, 1, false}
)

type Error struct{ Category, Option string }

func (e *Error) Error() string { return e.Category + ": " + e.Option }

func Parse(spec Spec, value string) (int, error) {
	if value == "" {
		return spec.Default, nil
	}
	if value == "off" {
		return 0, nil
	}
	if spec == Diagnostics && value == "on" {
		return 1, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > spec.Maximum {
		return 0, &Error{"invalid_option", spec.Name}
	}
	return n, nil
}

// Resolve uses all discovered participants, capped at sixteen servers and the
// parent's remaining 100ms budget. Missing servers are independently proved.
func Resolve(ctx context.Context, spec Spec) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	servers, err := tmux.ServersLimitContext(ctx, MaxServers)
	if err != nil {
		return 0, &Error{"policy_unavailable", spec.Name}
	}
	if len(servers) > MaxServers {
		return 0, &Error{"policy_capacity", spec.Name}
	}
	values := make([]int, 0, len(servers))
	for _, srv := range servers {
		value, err := srv.GlobalOptionContext(ctx, spec.Name)
		if err != nil {
			absent, probeErr := srv.Absent(ctx)
			if probeErr == nil && absent {
				continue
			}
			return 0, &Error{"policy_unavailable", spec.Name}
		}
		n, err := Parse(spec, value)
		if err != nil {
			return 0, err
		}
		values = append(values, n)
	}
	if ctx.Err() != nil {
		return 0, &Error{"policy_unavailable", spec.Name}
	}
	return combine(spec, values), nil
}

func combine(spec Spec, values []int) int {
	if len(values) == 0 {
		return spec.Default
	}
	n := values[0]
	for _, v := range values[1:] {
		switch {
		case v == 0 || n == 0:
			n = 0
		case spec.Longest:
			n = max(n, v)
		default:
			n = min(n, v)
		}
	}
	return n
}
