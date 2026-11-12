// Package version holds the client's release version and the contracts it
// speaks.
package version

// Version is the tmag-client release version. A client/vX.Y.Z tag carries the
// same value.
const Version = "0.1.0"

// The public contracts the client speaks: session state, watch and control
// documents, and a tmag serve endpoint's HTTP API.
const (
	State = "tmag/v2"
	HTTP  = "tmag/http/v2"
)
