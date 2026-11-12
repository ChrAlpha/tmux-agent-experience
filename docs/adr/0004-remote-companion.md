# ADR 0004: The remote companion in this repository

- Status: accepted
- Date: 2026-11-12
- Supersedes: ADR 0003's clause that moves UI, mobile experiences,
  multi-machine inventory and SSH/container aggregation to an independent
  `tmag-client` repository, and its last paragraph, the client's own browser
  admission and tunnel adapters.

ADR 0003 put the client in a repository of its own. That repository was never
published, and it could not follow `tmag/v2`; a client's correctness can only
be shown against the binaries and fake agents this repository builds, so a
contract change and the client's adaptation to it are one change here.

**R1. One repository.** The client is the Go module
`github.com/ChrAlpha/tmux-agent-experience/client`, in `client/`. It requires
no other module, imports nothing of the root module, and reads tmag only
through `tmag/v2` and `tmag/http/v2`. Its tests build the root module's
binaries from the same commit and run them as external programs.

**R2. Its own releases.** The client has its own version and tags,
`client/vX.Y.Z`. Its releases are never GitHub's latest, so latest keeps
meaning tmag for people, `scripts/install.sh` and the pages workflow, and they
move no tmag or plugin version: Codex asks people to approve tmag's hooks
again for every plugin version. The tmux plugin does not install the client.

**R3. What remote is.** The client's gateway runs in a tmux window on one
machine, the hub. It follows each machine once, through a command that runs
tmag there (`ssh box tmag`) or an endpoint of `tmag serve`, and serves one
person's page for triage and action:

- an inbox in the order things wait on the person: requests to answer and
  turns not yet read, then agents at work, then idle ones, and apart those of
  machines it cannot reach;
- for each session its last reply, its pane's screen while it works, and in
  place of the prompt box its open request in the agent's own terms, a
  command, an edit, a question with its choices, a plan; answer, prompt and
  stop;
- notifications to paired devices.

Every action is one tmag verb, decided by tmag's gate on the machine. The
client offers no terminal, runs no other command, starts no agent (a new task
starts by prompting an agent, which starts peers as the peers skill teaches),
and stores no content: what it keeps of sessions is ids and the turns read.
Agents working across machines are not part of this decision. `tmag serve`
stays for integrations and machines no command reaches.

**R4. Devices pair with the gateway.** Each device holds a credential of its
own, issued by redeeming a code that works once within ten minutes, from the
gateway's window, `tmag-client auth pair`, or a paired device, as text or a QR
code. The gateway keeps digests and metadata, lists devices, and revokes any
one, which ends its streams and drops its push subscription. The gateway
listens on loopback; a tunnel of the person's choice reaches it, and is named
with `--public`. A request is admitted by its host and a device's credential;
the gateway checks no tunnel's identity headers, which would add code and
flags per vendor and guard nothing a credential does not. Gateway credentials
are distinct from those `tmag auth` issues for `tmag serve`.

**R5. Alerts are logged.** tmag logs an `Alert` event when a change is worth
telling the person about, blocked or finished, with whether they were at a
terminal that shows the session: what the bell and `@tmag-notify` already
decide on. It is an optional addition to `tmag/v2` and carries no state:

```json
{"seq":120,"time":"2026-11-12T08:00:00Z","agent":"claude","session":"1787a44e-…","event":"Alert",
 "alert":{"kind":"finished","turn":"00161c25-…","present":false}}
{"seq":121,"time":"2026-11-12T08:01:00Z","agent":"claude","session":"1787a44e-…","event":"Alert",
 "alert":{"kind":"blocked","request":{"id":"3f9c2a01b6d4","kind":"permission","tool":"Bash"},"present":true}}
```

A client discards an alert the current state contradicts: a blocked alert
whose request closed, a finished alert whose session is no longer idle on
that turn. The companion pushes the rest, when recent and not present, to
devices whose page is not on screen, with names and ids only, through the
browsers' push services; a notification opens the session and answers
nothing.

The boundary ADR 0003 drew stays: clients read versioned public contracts and
import no internals, and their behaviour is tested from outside.
