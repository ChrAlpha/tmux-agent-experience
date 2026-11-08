package contextio

import (
	"context"
	"errors"
	"reflect"
)

// WithoutContextError removes only the selected canonical context leaf and
// wrappers whose causes consist solely of it. Joined storage, integrity and
// different cancellation errors survive even when errors.Is matches target.
// Callers decide whether their own budget, rather than the parent, expired.
func WithoutContextError(err, target error) error {
	// Match canonical stdlib leaves, rather than trusting a custom Is method
	// to claim that an I/O error is only a budget signal.
	validTarget := reflect.TypeOf(target) == reflect.TypeOf(context.Canceled) && errors.Is(target, context.Canceled) || reflect.TypeOf(target) == reflect.TypeOf(context.DeadlineExceeded) && errors.Is(target, context.DeadlineExceeded)
	if !validTarget {
		return err
	}
	remainder, _ := withoutContextError(err, target)
	return remainder
}

func withoutContextError(err, target error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if reflect.TypeOf(err) == reflect.TypeOf(target) && errors.Is(err, target) {
		return nil, true
	}
	switch wrapped := err.(type) { //nolint:errorlint // Dispatch the immediate node; errors.As would skip unwrap-tree branches.
	case interface{ Unwrap() []error }:
		changed := false
		var remaining []error
		for _, cause := range wrapped.Unwrap() {
			rest, removed := withoutContextError(cause, target)
			changed = changed || removed
			if rest != nil {
				remaining = append(remaining, rest)
			}
		}
		if changed {
			return errors.Join(remaining...), true
		}
	case interface{ Unwrap() error }:
		if rest, changed := withoutContextError(wrapped.Unwrap(), target); changed {
			return rest, true
		}
	}
	return err, false
}
