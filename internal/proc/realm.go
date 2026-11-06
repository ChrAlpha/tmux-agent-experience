package proc

import (
	"crypto/sha256"
	"encoding/hex"
)

// Realm names the process table this process sees. tmag proves what it knows
// from that table, so what one realm records means nothing in another:
// containers that share a home directory each see their own, and every boot
// starts a new one. It is "" when the platform does not say.
func Realm() string { return realmID(realmSource()) }

// realmID shortens what identifies a realm to a name that fits in a socket
// path, or "" for nothing.
func realmID(source string) string {
	if source == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:6])
}
