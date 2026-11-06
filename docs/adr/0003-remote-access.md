# ADR 0003: Remote access through tmag serve, alerts through @tmag-notify

- Status: accepted
- Date: 2026-11-06
- Supersedes: the "Remote access" section of
  [ADR 0001](0001-agent-control-surface.md), which had tmag serve nothing.
- Builds on: ADR 0001's verbs and gates, unchanged.

## Context

The person runs agents in tmux on a machine they are not always at. Away
from it, they need to learn that an agent waits for them, see exactly what it
asks, decide that one request, give the next prompt and read the reply, all
from a phone. A terminal over SSH does this badly on a small screen, and the
agents' own remote paths cover one vendor each: Claude Code's Remote Control
needs a claude.ai subscription login and keeps the transcript on Anthropic's
servers; Codex's goes through the ChatGPT apps and OpenAI's relay. Herdr, the
nearest tool that knows agents, replaces tmux, answers permission prompts only
by pressing keys, has no mobile client of its own and sends no notifications.

tmag already has what such a client needs: the `tmag/v1` contract, `watch`
with resumable sequence numbers, and gated verbs whose `request` shows a
request's tool and input. `ssh host tmag watch` serves a native client, but a
phone's browser speaks HTTP, and a client that is not running cannot tell the
person anything. Two things were missing: something that serves the contract
to one person's browser, and something that reaches the person when nothing of
theirs is open. A page written as an example against `tmag watch` showed that
the contract is all such a client needs.

## Decision

### `tmag serve`: one long-running command, inside tmux

`tmag serve` serves a page for one person's phone, and the API it uses, for as
long as it runs. It is the only tmag command that runs until stopped, and it
runs in a tmux window: `prefix + M-r` opens it (or shows where it already
runs), closing the window stops it. Nothing else is resident: the invariant
"anything resident is out of scope" becomes "nothing resident outside tmux".

- **It relays the contract.** Each page connection gets one `tmag watch` per
  machine, its lines passed on unchanged as server-sent events, resumed with
  `--since` after a disconnect. Each action runs one tmag command (`send`,
  `interrupt`, `answer`, `request`, `read`), which checks its own gate, so the
  page can do exactly what a person at the terminal can do through tmag and
  nothing more. It types no keys and opens no shell: an agent's terminal stays
  in tmux, reached over SSH when needed.
- **Machines are commands.** The local machine is serve's own binary; another
  is the command that runs tmag there, such as `ssh box tmag` or
  `docker exec -i dev tmag`. Nothing runs on those machines beyond those
  commands, for as long as a page watches.
- **It stores nothing** but the token that admits the person's devices, in
  the state directory.
  A request's input, a reply and a pane's text are fetched when the page asks
  and kept only in the page's memory.

### Who gets in

serve listens on loopback only, so every request arrives as a connection from
the machine itself, whether a browser here made it or a tunnel the person set
up forwarded it. To such a listener a header proves nothing by itself: any
process that reaches loopback, another user's included, can name a tunnel's
host and set the tunnel's headers. So:

- **Every request for data carries the token** in a header, `X-Tmag-Token`.
  The token is a secret in the state directory. The browser on this machine
  gets it in the address serve prints, after `#`, which the browser keeps to
  itself. A phone gets it by **pairing**: it sends a six-digit code that
  serve's window shows, and receives the token. A code pairs one device,
  lasts ten minutes, and gives way to a new one after five wrong tries, each
  shown in the window, so only someone who can see that window pairs a
  device. The page keeps the token in its origin's storage and sends it as a
  header: no other page, and no other port of the machine, can make the
  browser send it, and no other site can make it be sent at all.
- **A tunnel adds its proof of the owner.** A request under a host name
  Tailscale Serve forwards must carry the owner's login in
  `Tailscale-User-Login`, which tailscaled sets for tailnet users and removes
  from whatever a client sends; a tagged device or a Funnel request carries
  none. One under a host name a Cloudflare tunnel forwards behind Access must
  carry a `Cf-Access-Jwt-Assertion` that verifies (RS256) against the team's
  published keys, names the application's audience tag and the team as
  issuer, is current, and carries the owner's email. The plain
  `Cf-Access-Authenticated-User-Email` header is not trusted: a host name the
  tunnel forwards without an Access application would pass it on from anyone.
- A request with a loopback host name but any forwarding header is not local,
  and any other host name is refused, which keeps DNS rebinding out. Actions
  and pairing further take only a JSON body from the page's own origin.

The page itself holds no data and is served on these checks of the way in
alone, so that it can ask for a code; the manifest and icons, which browsers
fetch without credentials, are served to anyone under a known host name.
There are no accounts and no sharing: one person, the owner. Deleting
`serve.token` unpairs every device.

### Alerts reach the person through `@tmag-notify`

A hook already decides when a change is worth a bell (`@tmag-alert`: a session
starts waiting for the person, or finishes a turn). The same alerts run the
person's `@tmag-notify` command, for every session, unless a pane shows the
session on a tmux server whose clients had the person's input in the last
minute: then they are at that terminal, and the bell is there. A session no
pane shows, a Codex daemon thread or a Claude Code background session, rings
no bell anywhere, so its alerts always run the command.

- The hook starts `tmag notify` detached and returns; that process runs the
  command with `sh -c`, ends it after 20 seconds, and records a failure in
  `hook.log`, as hooks do. A slow network never holds up an agent.
- The command gets the alert in environment variables, never inside its own
  text: agent, session, state, kind, the waiting request and its tool, the
  session's directory, the pane's target, the host name, a title and message
  composed from those, and `TMAG_URL`, the request's page in a running serve
  reached through a tunnel. Never a prompt, reply or tool input: a
  notification goes to a third party, and a lock screen is no place to approve
  a command unseen. The page shows the input when the person opens it.
- `tmag notify` run by hand sends a test alert and reports what the command
  did.

### A watch ends when nobody reads it

A client over SSH that loses its connection leaves `tmag watch` on the host
with nothing to write, so it would run until the next event. `watch` now ends
as soon as its output's reader goes away: the reader of a pipe closed it, or a
socket was shut down both ways (sshd runs a command without a terminal on one
socket, and passes on the end of the client's input as a half-close, which is
no hang-up).

## Consequences

- The person reaches every Claude Code and Codex session from a phone,
  including Codex daemon threads and Claude Code background sessions, through
  a tunnel of their own, and answers the request they see, with the same
  gates and the same event log as at the terminal.
- tmag now has one network-facing command. It is off until the person opens
  it, listens on loopback, and admits one person by a proof the tunnel or the
  machine gives; the verbs remain the only actions.
- Notifications need a service the person chooses (ntfy, Bark, Pushover,
  Telegram); tmag sends them nowhere of its own.
- A session on another machine gets its notification from that machine's
  hooks; its `TMAG_URL` names a serve only when one runs there.

## Alternatives considered

- **Keep the page as an example.** Users only ever get released binaries, so
  a page built from source is no product, and a second binary would add a
  second install and a version to keep equal. The binary already links
  `net/http`; the page adds little to it.
- **A background service** (systemd, launchd). It would be resident outside
  tmux, invisible to the person who did not start it, and outlive the
  sessions it is about.
- **Web Push from serve.** It needs serve running whenever an alert happens
  and the subscriptions stored. iOS shows no action buttons on web
  notifications, so the person opens the page either way; a hook reaches them
  whether or not serve runs.
- **Running the notifier with `tmux run-shell -b`.** tmux would expand formats
  in the command, may show its output in a pane, and needs a server the hook
  can reach; a detached tmag needs none of that.
- **A terminal in the page.** Keys typed from a phone bypass every gate, and a
  shell in a browser is remote access to the machine, which is more than the
  person needs to answer an agent. tmux over SSH remains the terminal.
- **Relaying transcripts.** ADR 0001 keeps content in each agent's format and
  where the agent writes it; the page reads a turn's reply with `read --last`
  like any client.
- **Trusting a tunnel's identity header alone**, Tailscale's login or
  `Cf-Access-Authenticated-User-Email`: anyone who reaches loopback can send
  either. See "Who gets in".
- **The token in a cookie.** Browsers send a cookie to every port of a host
  and with requests other sites start, so another service on the machine
  would receive it, and a page elsewhere could start a watch through the
  tunnel with it.
- **Proving the tunnel by the connecting process** (its user, from the
  socket table). Containers that share the host's network commonly run as
  the same user id, and macOS has no cheap way to tell; a secret the person
  carries works the same everywhere.
- **A heartbeat line in `watch`** to notice a dead reader. It would add a
  document type to the contract that every client must skip; the hang-up is
  visible without writing anything.
