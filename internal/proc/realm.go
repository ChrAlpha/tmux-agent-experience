package proc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Realm names the process table this process sees. tmag proves what it knows
// from that table, so what one realm records means nothing in another:
// containers that share a home directory each see their own, and every boot
// starts a new one. It is "" when the platform does not say.
func Realm() string { return realmID(realmSource()) }

// RealmContext resolves the realm within the operation's discovery budget.
func RealmContext(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source, err := realmSourceContext(ctx)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return realmID(source), nil
}

// realmID shortens what identifies a realm to a name that fits in a socket
// path, or "" for nothing.
func realmID(source string) string {
	if source == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:6])
}
