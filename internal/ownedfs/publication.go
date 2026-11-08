package ownedfs

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const manifestTemporarySlots = 256

func manifestTemporaryName(slot int) string {
	if slot == 0 {
		return ".tmag-scope.tmp"
	}
	return fmt.Sprintf(".tmag-scope.tmp.%02x", slot)
}

func validManifestTemporary(name string) bool {
	for slot := range manifestTemporarySlots {
		if name == manifestTemporaryName(slot) {
			return true
		}
	}
	return false
}

// manifestTemporary reuses a name only after a committed manifest reserved it.
// Before that first commit, a crashed creator is indistinguishable from an
// unknown collision, so preserve it and try the next fixed candidate. This
// bounds unproven bootstrap leftovers to 256 positions without making one
// ordinary first-bootstrap crash an outage. If all positions are occupied,
// proof is unavailable: fail closed and leave their resolution to the owner.
// The caller holds the scope gate until publication completes or is abandoned.
func (d *Dir) manifestTemporary(ctx context.Context, established string) (*os.File, string, error) {
	if established != "" {
		if !validManifestTemporary(established) {
			return nil, "", ErrScope
		}
		if st, err := d.Stat(ctx, established); err == nil {
			if !st.Mode().IsRegular() {
				return nil, "", ErrScope
			}
			if err := d.Remove(ctx, established, st); err != nil {
				return nil, "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		f, err := d.open(ctx, established, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		return f, established, err
	}
	for slot := range manifestTemporarySlots {
		name := manifestTemporaryName(slot)
		if _, err := d.Stat(ctx, name); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		f, err := d.open(ctx, name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, name, err
	}
	return nil, "", ErrScope
}
