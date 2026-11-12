# tmag-client

The remote companion: a mobile-first page for one person's agents in tmux,
and the gateway that serves it from one machine, the hub.

The gateway reads each machine through public contracts only: `tmag/v2` for a
command such as `ssh box tmag`, and `tmag/http/v2` for an HTTP endpoint of
`tmag serve`. It follows each machine once for its whole life, keeps each
machine's sessions and panes in memory, and serves every page from that state:
a page gets a snapshot, then events. A machine that cannot be reached shows as
offline and is retried with backoff. Every action is one tmag verb, whose own
control gate runs on the machine. The page shows requests with their tool
input, a session's last reply and its pane's recent text; it has no terminal
and runs no other command.

The client is a module of this repository (`client/`). It requires no other
module, imports nothing of the root module, and ships vanilla browser assets
with no build step. Its releases carry their own version, tagged
`client/vX.Y.Z`; they are never marked latest, so latest keeps meaning tmag.

## Build and run

```sh
cd client
go build -o bin/tmag-client ./cmd/tmag-client
bin/tmag-client serve
```

`serve` runs in the foreground, listening on loopback `127.0.0.1:7778` by
default. It prints an address whose fragment carries one fresh invitation:

```
tmag-client at http://127.0.0.1:7778/#pair=XXXX-XXXX-XXXX
```

Opening the address pairs that browser. The invitation is single-use and lasts
ten minutes; another device pairs with `auth pair` below. Without a backend
`serve` runs `tmag` from `PATH` as the machine `local`.

```sh
bin/tmag-client --version   # tmag-client 0.1.0 (tmag/v2, tmag/http/v2)
```

## Machines

- `--machine NAME=COMMAND` supplies a command backend, such as
  `--machine 'box=ssh box tmag'` or
  `--machine 'dev=docker exec -i dev tmag'`. Quote the value when the command
  has spaces. It must work noninteractively. Each machine is watched once by
  the gateway; each action invokes one gated tmag verb.
- `--endpoint NAME=URL` supplies an HTTP endpoint of `tmag serve`. Pair it
  with `--credential NAME=FILE`, where FILE holds the issued token or the JSON
  returned by `tmag auth issue --json`. Loopback HTTP and remote HTTPS are
  supported. The adapter sends bearer credentials in headers, refuses
  redirects, preserves command errors, and unwraps the single-instance SSE
  contract.

The two transports may be mixed. Names must be unique. HTTP credentials
belong to the respective tmag instances and can be revoked independently
there.

Both transports need tmag 0.7.0 or later on each machine for push and seen:
an older tmag reports no `Alert` or `Seen` events, so its finished turns stay
in the inbox and marking one fails with that tmag's error.

## Devices

Each device pairs with the gateway and holds a credential of its own. The
credential lives in the page's `localStorage` and goes with every request in
the `Authorization: Bearer` header, never in a URL or a cookie. On iOS, add the
page to the home screen before pairing and pair from there: a home-screen web
app keeps its own storage, so a device paired in Safari is not paired in the
app.

```sh
bin/tmag-client auth pair LABEL [--ttl D] [--json]
bin/tmag-client auth list [--json]
bin/tmag-client auth revoke ID
```

`auth pair` prints a single-use invitation valid for ten minutes; redeeming it
issues a credential labelled `LABEL`. `--ttl` limits that credential's life;
without it the credential lasts until revoked. `auth list` shows ids, labels,
created and expiry times, and whether push is on. `auth revoke ID` removes the
credential, closes its open streams and drops its push subscription. The
commands work on the state directory whether or not `serve` runs, and a
running gateway notices within a moment. They use the same
`--state-dir`/`TMAG_CLIENT_STATE_DIR` as `serve`.

The state directory, `$XDG_STATE_HOME/tmag-client` by default, holds
`credentials.json` with credential digests, push subscriptions and metadata,
`vapid.json` with the gateway's Web Push keys, and `lock`; its files are
readable by its owner only.

To reach the page through a tunnel, use `--tailscale HOSTS --owner LOGIN`, or
`--access HOSTS --access-team TEAM --access-aud AUDIENCE --owner EMAIL`. A
request must then also carry the tunnel's proof of the owner. Tunnel setup,
accounts and deployment are supplied externally.

## Inbox

Sessions sort into the inbox (blocked, and idle on a turn the person has not
seen), working, settled (idle on a seen turn, or idle without a turn), then
sessions whose state is unknown; longest waiting first inside each group.

A turn is seen when the person watches it at the terminal, when the plugin's
tmux hooks see its pane come into view, or when `tmag seen` marks it. Opening
a session whose turn has finished runs `tmag seen` on that machine for the
turn the view shows, and a turn that ends while its view is open is marked
the same way; the `Seen` event that follows settles it on every device, and
a failure shows on the page. The seen turn belongs to the session's record,
so the gateway keeps no session state of its own and a restart changes
nothing.

The page's routes are `#/` for the inbox, `#/s/<session id>` for one session,
and `#/s/<session id>/r/<request id>` for one request.

## Push

```sh
bin/tmag-client serve --tailscale box.example.ts.net --owner me@example.com
```

`--push` chooses the alert kinds to push, comma-separated (default
`blocked,finished`; an empty value turns push off). Push needs one of the
tunnel hosts above: the VAPID subject is `https://` plus the first public host,
and a page reached on loopback says push needs a tunnel instead of offering
it. A push endpoint must be on a browser's push service: `fcm.googleapis.com`
for Chrome and other Chromium browsers, `updates.push.services.mozilla.com`
for Firefox, `web.push.apple.com` for Safari, or a host under
`notify.windows.com` for Edge. `--push-service HOST` adds an exact self-hosted
push service; repeat it as needed. A device turns notifications on from its
page; the gateway keeps its subscription with that device's credential and
sends to every subscribed device except one whose page is visible within the
last minute.

Alerts come from tmag's `Alert` events. The gateway discards one whose current
state contradicts it, and sends metadata only: kind, machine, agent, session,
request, tool and the directory's base name, in a title and body worded like
tmag's own notification. The service worker shows it tagged by session and
opens the session or request when tapped; it caches nothing. On iOS, the
home-screen app is also the one that receives notifications.

## Test

```sh
cd client
go test -race ./...
golangci-lint run
for f in internal/gateway/assets/*.js; do node --check "$f"; done
go list -m all   # the client module alone
```

Tests use temporary files, fake command backends, `httptest` servers and a
fake push service that decrypts a message and verifies its VAPID token. A test
that needs tmux uses a private server under a temporary `TMUX_TMPDIR`. The
end-to-end tests build `tmag`, the fake agent and `tmag-client` from the tested
commit, then drive pairing, the inbox, actions, push and revocation against
them.
Its research on remote agent access is
[docs/research/herdr.md](../docs/research/herdr.md).
