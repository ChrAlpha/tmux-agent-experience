# tmag-client

The remote companion: the coding agents in your tmux, on your phone. Its
gateway runs in a tmux window on one machine, follows the machines you name
through tmag's public contracts, and serves one page for triage and action:

- **The inbox.** Agents waiting on you come first: an approval, a question or
  a plan to answer, and turns that finished since you last looked. Then the
  agents at work, then the idle ones. Agents on a machine the gateway cannot
  reach are kept apart, because their state may be stale.
- **A session.** Its last reply, its pane's screen while it works, and at the
  bottom either its open request, shown as the agent will act on it (the
  command, the file and its change, the question and its choices, the plan),
  or a box to prompt it. Stop is in both.
- **Notifications.** When an agent needs you or finishes while you are not at
  a terminal that shows it, paired devices are told, unless they have the page
  on screen, where the alert shows in place. A tap opens the session; a
  notification never answers anything.

Every action is one tmag verb, which tmag's own gate decides on the agent's
machine. The gateway stores no content: replies, screens and tool input go
from the machine to the page that asked, and push messages hold names and
ids. There is no terminal, and the gateway runs no other command.

The client is a Go module of this repository with its own version and
releases, tagged `client/vX.Y.Z`; the tmux plugin does not install it.

## Run it

In a tmux window of the machine that is to be the hub:

```sh
tmag-client serve --public https://laptop.example.ts.net --machine laptop --machine box='ssh box tmag'
```

```
tmag-client 0.1.0 at http://127.0.0.1:7778, reached at https://laptop.example.ts.net
  following laptop: /home/me/.tmux/plugins/tmux-agent-experience/bin/tmag
  following box: ssh box tmag

Pair a device: scan this code, or open https://laptop.example.ts.net/#pair=7H9N-HNEA-J59F
```

A QR code of that address follows, while no device is paired yet. The gateway
runs until the window closes or you press Ctrl-C.

- `--machine NAME=COMMAND` follows a machine through a command that runs tmag
  there and works without a terminal: `ssh box tmag`, `docker exec -i dev
  tmag`. Through ssh, use the full path when tmag is not on the remote
  `PATH`: `box='ssh box ~/.tmux/plugins/tmux-agent-experience/bin/tmag'`.
  `--machine NAME` alone is this machine's tmag, on `PATH` or in the tmux
  plugin's directory. Without any machine, the gateway follows this one,
  named after its host.
- `--endpoint NAME=URL --credential NAME=FILE` follows a machine through
  `tmag serve`, for one no command reaches. FILE holds a credential from that
  machine's `tmag auth issue LABEL --json`, or its token alone.
- `--addr` is the loopback address the gateway listens on, `127.0.0.1:7778` by
  default. Plain HTTP never leaves the machine: a tunnel reaches it.
- `--public URL` is an https origin a tunnel serves the gateway at. The page is
  reached there, codes pair devices there, and notifications need it. Repeat
  it for each tunnel.

A watch that breaks is tried again, after a second and then up to half a
minute apart, and resumes after the last event the gateway applied. Give ssh a
keepalive (`ServerAliveInterval`) so that a connection that died is noticed.

## Reach it from elsewhere

The gateway listens on loopback; a tunnel you choose makes it reachable and
gives it https. With Tailscale Serve, on the hub:

```sh
tailscale serve --bg 7778
```

then start the gateway with `--public` and the address Tailscale prints. A
Cloudflare Tunnel, or any reverse proxy that terminates TLS and forwards to
`127.0.0.1:7778`, works the same way. The gateway admits requests whose host is
loopback or a `--public` origin's, and every request but pairing must carry a
paired device's credential.

## Pair a device

Each device pairs with a code that works once, within ten minutes, and gets a
credential of its own:

- scan the QR code `serve` prints, or open its address;
- or run `tmag-client auth pair phone` in another window of the hub, which
  prints the code and, once `serve` has run, its address and QR code;
- or, on a device already paired, open Settings and get a code there.

On iPhone and iPad, tmag lives on the Home Screen: open the address in Safari,
tap Share, then Add to Home Screen, open tmag from there and enter the code. A
Home Screen app keeps storage of its own, so pairing in Safari does not pair
the app, and notifications come only to the app.

```sh
tmag-client auth list           # id, label, when it paired, until when, and notifications
tmag-client auth revoke ID      # unpair one device
```

Unpairing takes effect at once: the device's page closes and it gets no more
notifications. A device can also be unpaired from Settings on any paired one.
`auth pair --ttl 720h` gives the credential a life; without one it lasts until
revoked.

## Notifications

Turn them on under Settings, on each device. The gateway pushes an alert that
tmag logged, `blocked` or `finished`, when:

- nobody was at a terminal that shows the agent, as tmag decides for the bell
  and `@tmag-notify`;
- the agent's state still agrees: the request is open, the turn not read;
- and the device's page is not on screen.

`--push blocked` pushes only requests; `--push ""` pushes nothing. Messages go
through the browser's push service (Apple's, Google's, Mozilla's or
Microsoft's) encrypted for the device, and only to those services;
`--push-service HOST` adds one, such as a self-hosted push server.

## The state directory

`$TMAG_CLIENT_STATE_DIR`, or `$XDG_STATE_HOME/tmag-client`
(`~/.local/state/tmag-client`), readable by its owner alone; `--state-dir`
names another.

| File | Holds |
| --- | --- |
| `devices.json` | Each device's id, label, times, credential digest and push subscription; invitation digests |
| `seen.json` | The last turn read of up to 512 sessions: ids and turn ids |
| `vapid.json` | The gateway's push key |
| `address` | The address devices pair at, for `auth pair` |

## Build from source

```sh
cd client
go build -o bin/tmag-client ./cmd/tmag-client
bin/tmag-client --version   # tmag-client 0.1.0 (tmag/v2, tmag/http/v2)
```
