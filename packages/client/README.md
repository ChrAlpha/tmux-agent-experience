# tmag-client

An independent web client and optional multi-machine gateway for tmag. Extracted
from `tmux-agent-experience` branch `feat/remote`, commit `1ceda5e`, on 2026-11-07.
The MIT license and original copyright are retained.

The local tmux plugin, hooks, agent state, control gates, HTTP endpoint and owner
credentials belong to `tmux-agent-experience`. This repository owns the browser
UI, PWA assets, navigation, browser admission, tunnel integrations and aggregation.
It depends only on public `tmag/v1` CLI and `tmag/http/v1` contracts and has no Go
module dependency on tmag internals. Its release cadence is independent.

Build with `go build -o bin/tmag-client ./cmd/tmag-client`. Run `go test ./...` and
`go test -race ./...` for the gateway and public HTTP adapter. Go standard library
only. A local tmag executable on PATH is the default backend. The process runs in
the foreground, and may be hosted in a tmux window.

## Connecting instances

- `--machine NAME=COMMAND` supplies a command backend, such as `box=ssh box tmag`
  or `dev=docker exec -i dev tmag`. It must work noninteractively. Each subscriber
  owns a watch per machine; each action invokes one gated tmag verb.
- `--endpoint NAME=URL` supplies a local tmag HTTP endpoint. Pair it with
  `--credential NAME=FILE`, where FILE contains the issued token or the JSON
  returned by `tmag auth issue --json`. Local loopback HTTP and remote HTTPS are
  supported. The adapter sends bearer credentials in headers, refuses redirects,
  preserves command errors, and unwraps the single-instance SSE contract.
- The two transports may be mixed. Names must be unique. HTTP credentials belong
  to the respective tmag instances and can be revoked independently there.

## Browser admission

The gateway itself listens on loopback, port 7778 by default. Its printed local
URL includes a browser credential in the fragment; keep that URL private. Its
own authorization directory is `XDG_STATE_HOME/tmag-client`, overridable with
`--state-dir`. This browser credential is separate from credentials for upstream
tmag HTTP endpoints. The extracted browser admission currently shares one token
across browsers; deleting `gateway.token` and restarting unpairs all browsers.

To access through a tunnel, use `--tailscale HOSTS --owner LOGIN`, or
`--access HOSTS --access-team TEAM --access-aud AUDIENCE --owner EMAIL`.
The gateway retains the extracted checks: a paired browser token plus the
tunnel's proof of the owner. Pair a remote browser using the displayed one-time
code. Tunnel setup, accounts and deployment are supplied externally.

The page reads request details and replies on demand; the gateway keeps no
transcript or prompt history. Each operation still passes the target instance's
control gates. Closing the page ends its subscriptions; accepted actions finish.
The gateway only exposes its configured backend names and known tmag verbs.

## Notifications and compatibility

A notifier can build client links using exported `TMAG_SESSION` and
`TMAG_REQUEST` metadata. The page's routes live here; the local tmag foundation
has no knowledge of them. State events remain `tmag/v1`; the HTTP adapter checks
`tmag/http/v1`. Unsupported HTTP contracts fail explicitly.

`docs/research/herdr.md` retains the dated research that motivated the original
client exploration. It is historical research, not a current compatibility claim.
