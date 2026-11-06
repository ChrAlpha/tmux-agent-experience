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
- **It stores nothing** but a token for local access, in the state directory.
  A request's input, a reply and a pane's text are fetched when the page asks
  and kept only in the page's memory.

### Who gets in

serve listens on loopback only, so every request comes from the machine
itself or through a tunnel the person set up. Each request is one of:

- **Forwarded by Tailscale Serve** under a host name serve was given: it must
  carry the owner's login in `Tailscale-User-Login`. tailscaled removes any
  such header a client sends and sets its own for tailnet users only, not for
  tagged devices or Funnel, so the header proves who it is.
- **Forwarded by a Cloudflare tunnel behind Access** under a host name serve
  was given: its `Cf-Access-Jwt-Assertion` must verify (RS256) against the
  team's published keys, name the application's audience tag and the team as
  issuer, be current, and carry the owner's email. The plain
  `Cf-Access-Authenticated-User-Email` header is not trusted: a host name the
  tunnel forwards without an Access application would pass it on from anyone.
- **Local**: a loopback host name and no forwarding header at all. It must
  carry serve's token, given once as `?token=` in the URL serve prints and
  kept as a strict same-site cookie: other users of the machine can reach
  loopback too.

Anything else is refused, which also keeps DNS rebinding out. Actions further
need a same-origin JSON request with a custom header, which no other site can
make a browser send. There are no accounts and no sharing: one person, the
owner.

### Alerts reach the person through `@tmag-notify`

A hook already decides when a change is worth a bell (`@tmag-alert`: a session
starts waiting for the person, or finishes a turn). The same alerts run the
person's `@tmag-notify` command, for every session, including those no pane
shows, unless the person typed into a tmux client in the last minute: then
they are at the terminal, and the bell is there.

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
- **Trusting `Cf-Access-Authenticated-User-Email`** or any identity header
  without the tunnel's proof: see "Who gets in".
- **A heartbeat line in `watch`** to notice a dead reader. It would add a
  document type to the contract that every client must skip; the hang-up is
  visible without writing anything.
