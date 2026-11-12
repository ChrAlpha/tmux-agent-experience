# client/AGENTS.md

Guidance for changing the remote companion. README.md here introduces it; the
root AGENTS.md describes the repository it belongs to, and its invariants hold
here too.

## Place in this repository

The client is the Go module `github.com/ChrAlpha/tmux-agent-experience/client`.
It reads tmag only through public contracts: the `tmag/v2` documents of tmag's
command line, and `tmag/http/v2`. It requires no other module and imports
nothing of the root module. Its tests build the root module's binaries from
the same commit and run them as external programs, as people run them.

Sessions, attachments, transcripts and the control gates belong to each tmag.
The client owns the page, the devices paired with it, the machines it follows
and what it shows of them.

## Promises

- **One follower per machine.** The gateway watches each machine once, for
  every page, and resumes after the last event it applied. A page never
  starts a watch; it gets the views that change.
- **Pages apply nothing.** The gateway turns tmag's documents into views (a
  session, the panes that show it, whether its turn waits to be read) and
  sends each one that changed in full. A page keeps the latest view of each
  session.
- **A turn is read only while the session is idle on it**, so a turn still
  running is never taken as read. Read turns are ids, the last 512.
- **Every action is one tmag verb**, run to its end even when the page that
  asked goes away, and its refusal comes back with tmag's code and `next:`.
- **Alerts are tmag's.** The gateway pushes an `Alert` event only when the
  session's state still agrees with it, when it is recent, when nobody was at
  a terminal that shows the session, and to devices whose page is not on
  screen. A notification only opens the session.
- **No content is stored.** Replies, screens and tool input go from a machine
  to the page that asked; the state directory holds ids, digests and keys,
  push messages names and ids, and the service worker nothing.
- **Admission.** Requests are admitted by host (loopback, or a `--public`
  origin's) and, but for pairing, by a paired device's credential in the
  `Authorization` header, never in a URL or a cookie. Codes work once, for ten
  minutes, and failed pairings are limited. A revocation from any process ends
  the device's streams within a second.
- **Push goes to push services.** An endpoint is an https URL of a browser's
  push service, or of one `--push-service` names; it is checked when a device
  subscribes and again before each message.
- Go standard library only; the page is HTML, CSS and scripts with no build
  step and no request beyond the gateway.

## Layout

| Path | Role |
| --- | --- |
| `cmd/tmag-client` | Commands, flags and output |
| `internal/gateway` | Following machines, views, read turns, alerts and pushes, the page's API and its files |
| `internal/gateway/assets` | The page: `index.html`, `app.js`, `app.css`, the service worker `sw.js`, the manifest |
| `internal/machine` | A machine through a command or a `tmag serve` endpoint: watch, one verb, tmag's documents |
| `internal/devices` | Paired devices, their credentials as digests, push subscriptions, invitations |
| `internal/webpush` | Web Push: aes128gcm, VAPID, the known push services; `webpushtest` is a browser's end |
| `internal/qr` | QR codes of pairing addresses, for terminals and the page |
| `internal/e2e` | End-to-end tests against tmag and the fake agent |

## Commands

```sh
cd client
go build ./...
go vet ./...
go test -race ./...           # unit + e2e; e2e needs tmux and skips without it
golangci-lint run             # the root .golangci.yml
for f in internal/gateway/assets/*.js; do node --check "$f"; done
go list -m all                # the client module alone
govulncheck ./...
```

## Testing

- Unit tests run the gateway over a fake machine the test drives, a fake push
  service that decrypts what it gets, and `httptest` servers.
- The end-to-end tests run `tmag-client serve` in a window of a private tmux
  server under a temporary `TMUX_TMPDIR`, following the real tmag, beside the
  root module's fake agent (`FAKE_ANSWER=1`, `FAKE_INBOX=1`). The gateway's
  window is outside the test's process tree, so tmag sees a person act. On
  Linux the gateway trusts a fake push service through `SSL_CERT_FILE`.
- Web Push encryption is checked against RFC 8291's example, and QR codes
  against the Python qrcode library's (`internal/qr/testdata`).
- To look at the page, run the gateway over fake agents and open it in a
  browser at 390 by 844, light and dark.

The version lives in `internal/version/version.go`; a `client/vX.Y.Z` tag
carries the same value, and its release is never latest.
