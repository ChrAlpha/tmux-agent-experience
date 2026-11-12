# client/AGENTS.md

Guidance for changing the remote companion. README.md here introduces it; the
root AGENTS.md describes the repository it belongs to.

## Place in this repository

The client is the Go module
`github.com/ChrAlpha/tmux-agent-experience/client`, in `client/`. It consumes
only public contracts: `tmag/v2` CLI JSON documents and `tmag/http/v2`. It
requires no other module and imports nothing of the root module. A test may
build the root module's binaries from the same commit and run them as
external programs, as people run them.

State, agent detection, transcripts and the control gates belong to each tmag
instance. The client owns the page, its browser admission, the tunnel
adapters, machine configuration and aggregation.

## Promises

- Go standard library only.
- Vanilla browser assets, served from the binary, with no build step.
- Gateway credentials are distinct from the credentials `tmag auth` issues
  for `tmag serve`. Each paired device has one, sent in a header; the gateway
  keeps only digests, push subscriptions and metadata, in files its owner
  alone can read.
- No content is stored: the page reads replies, pane text and request input
  on demand, and keeps ids, names and times. Seen turns are ids and turn ids
  in `seen.json`; push payloads carry metadata only.
- Both credentials and tunnel proofs admit the owner; a tunnel's check never
  replaces the gateway's own.
- The gateway follows each machine once, for every page. Events are relayed
  from that state; a page never starts its own watch.
- Push is Web Push with VAPID and aes128gcm from the standard library. A push
  endpoint is an HTTPS URL whose host is a DNS name; the transport follows the
  environment's proxy, refuses redirects and is bounded in time. Failures stay
  metadata.

## Commands

```sh
cd client
go build ./...
go vet ./...
go test -race ./...
golangci-lint run              # the root .golangci.yml
for f in internal/gateway/assets/*.js; do node --check "$f"; done
go list -m all                 # the client module alone
```

Tests use temporary files, fake command backends and `httptest` servers. A
test that needs tmux uses a private server and a temporary `TMUX_TMPDIR`,
never the developer's server. A test that needs a real tmag builds it from
the root module at the tested commit. The end-to-end tests build `tmag`, the
fake agent and `tmag-client`, then drive pairing, the inbox, actions, push
and revocation against them. Push tests run a fake push service that decrypts
the message and verifies the VAPID token.

The version lives in `internal/version/version.go`; a `client/vX.Y.Z` tag
carries the same value. A client release moves no tmag or plugin version.
