# ADR 0004: A remote companion in this repository

- Status: accepted
- Date: 2026-11-12
- Supersedes: ADR 0003's clause that moves UI, mobile experiences,
  multi-machine inventory and SSH/container aggregation to the independent
  `tmag-client` repository, and ADR 0003's last paragraph, the client's own
  browser admission.

ADR 0003 kept this repository on the local foundation and put the client in
a separate repository. That repository was never published, and its main
branch speaks v1, so it cannot talk to core 0.4.0 or later; its v2 port never
merged. The client's correctness can only be tested against real tmag binaries
and the fake agents this repository's end-to-end tests already have, so a
contract change and the client's adaptation to it are one change here.

**R1. One repository.** The client is the Go module
`github.com/ChrAlpha/tmux-agent-experience/client`, in `client/`. It requires
no other module, imports nothing of the root module, and consumes only the
public contracts: `tmag/v2` CLI JSON documents and `tmag/http/v2`. Its tests may build
the root module's binaries (`tmag`, `internal/e2e/fakeagent`) from the same
commit and run them as external programs, as users run them.

**R2. One release train each.** The client has its own version and tags
(`client/vX.Y.Z`). Its GitHub releases are never marked latest, so latest
keeps meaning tmag for people, `scripts/install.sh` and the pages workflow. A
client release moves no tmag or plugin version: Codex asks people to approve
tmag's hooks again for every plugin version. People get only released,
checksummed client binaries, and the TPM plugin does not install the client.

**R3. What remote is.** The client's gateway runs in a tmux window on one
machine, the hub. It follows each machine once, through a command such as
`ssh box tmag` or `docker exec -i dev tmag`, or through an HTTP endpoint of
`tmag serve`, and serves a mobile-first web app for triage and action:

- an inbox of the sessions that need the person: blocked ones, and idle ones
  whose last turn the person has not seen; then working ones; then settled
  ones (idle, seen);
- for each session: its last reply, its pane's recent text, an open request's
  tool input, and answer, send and interrupt;
- Web Push alerts to paired devices.

The client offers no terminal and runs no arbitrary command. It stores no
content: what it remembers of sessions is ids and turn ids. A new task starts
by prompting an agent, which starts peers as the peers skill teaches. Machines
reached through a command need no `tmag serve`; `tmag serve` serves
integrations and machines no command reaches. Agents working across machines
are not part of this decision.

**R4. Admission.** Devices pair with the gateway, not with `tmag serve`. Each
device holds a credential of its own. The gateway keeps only its digest and
metadata in the gateway's state directory, issues it by redeeming a
single-use invitation valid for ten minutes, lists credentials and revokes
any one. Revocation closes that device's streams and drops its push
subscription. Tunnel proofs of the owner (Tailscale Serve, Cloudflare Access)
stack on top. Gateway credentials are distinct from those `tmag auth` issues
for `tmag serve`.

**R5. Alerts.** tmag logs an `Alert` event when a change is worth telling the
person about (blocked, finished), with whether they were at a terminal that
shows the session: the decision the bell and `@tmag-notify` already rest on.
Clients push from these events and choose which kinds. The event is an
optional addition to `tmag/v2`:

```json
{"seq":120,"time":"2026-11-12T08:00:00Z","agent":"claude","session":"1787a44e-…","event":"Alert",
 "alert":{"kind":"finished","turn":"00161c25-…","present":false}}
{"seq":121,"time":"2026-11-12T08:01:00Z","agent":"claude","session":"1787a44e-…","event":"Alert",
 "alert":{"kind":"blocked","request":{"id":"3f9c2a01b6d4","kind":"permission","tool":"Bash"},"present":true}}
```

The event carries no `state`, so it reports nothing about the session's
fields, like `Send` or `Halt`. A client discards an alert the current state
already contradicts: a blocked alert whose request is no longer open, a
finished alert whose session is no longer idle on that turn.

The client's command line is:

```
tmag-client serve [gateway flags] [--push KINDS]
tmag-client auth pair LABEL [--ttl D] [--json]
tmag-client auth list [--json]
tmag-client auth revoke ID
tmag-client --version
```

`serve` runs the gateway in the foreground, in a tmux window. `--push` names
the alert kinds it pushes (default `blocked,finished`; empty turns push off).
The internal HTTP worker invocation the gateway runs for HTTP backends stays
internal. The page's routes are `#/` for the inbox, `#/s/<session id>` for a
session and `#/s/<session id>/r/<request id>` for a request; a notification
opens one of them.

The boundary ADR 0003 drew stays: clients consume versioned public contracts
and import no internals. Their correct behaviour is tested from the outside,
against the binaries and fake agents this repository releases and runs.
