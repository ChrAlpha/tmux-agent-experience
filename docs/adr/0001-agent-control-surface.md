# ADR 0001: One control surface, a driver per agent and runtime

- Status: accepted; its "Remote access" section is superseded by
  [ADR 0003](0003-remote-access.md); amended 2026-11-10: a delivery's effect
  is recorded, then logged, best effort, and the contract is `tmag/v2`;
  amended again 2026-11-10: one `send` verb delivers now, with `--wait` for a
  turn of its own and `--reply` for an answer; `reply` uses the asker's
  channel
- Date: 2026-11-03
- Evidence: [Claude Code control channels](../research/claude-control-channels.md)
  (2.1.317), [Codex control channels](../research/codex-control-channels.md)
  (0.166), both observed live.

## Context

`tmag watch` gives a client every session's state and the panes that show it.
A client that also shows what a session is doing and acts on it (answers a
permission prompt from a phone, stops a runaway turn) needs three more
things: where the session's content is, a way to answer what the agent asks,
and a way to interrupt it. Until now `tmag send` was the only action, and it
typed into a pane.

The agents differ in how each of these can be done, and the differences move
with every release. Codex in its shared daemon can be driven over the
daemon's own protocol; Codex with `--no-daemon` and every interactive Claude
Code session only through the terminal; Claude Code's background sessions
have no terminal at all. Claude shows its permission dialog while hooks run;
Codex waits for hooks before it shows one.

If clients had to know all this, every agent release would change tmag and
every client. tmag must stay a low-intrusion tmux enhancement, not a general
agent protocol: tmux remains the entry point and the agents keep running
unwrapped.

## Decision

### The line: tmag unifies verbs, not content

tmag offers a fixed set of verbs, each with one meaning and one safety gate
across agents and runtimes. How a verb reaches a session is a driver's
business, chosen inside tmag per agent and runtime. Content keeps each
agent's own format: tmag says where it is and never reshapes it.

This is not ACP or A2A. Those define how a client launches and talks to an
agent and normalise its messages; that requires wrapping the agent and flattens
what makes each agent useful. tmag's verbs are only what a person at the
terminal can already do, made safe to do from elsewhere.

The same verbs serve agents acting on one another, run from their tools.
Prompting or stopping another session is what a person could type, so it
stays the same verb with the same gate; deciding a permission prompt is not:
it stands in for the person's consent. tmag therefore proves who runs a verb
as it proves which pane shows a session (see [Callers](#callers)).

### Verbs

| Verb | Acts when | Guarantees |
| --- | --- | --- |
| `send <target> <text>` | live session, nothing of tmag's unconfirmed | delivered now: an idle session starts a turn, a working one reads the prompt at its next step; its `Send` event precedes what follows |
| `send --wait <target> <text>` | session idle, no unconfirmed prompt | one prompt starts one turn; its `Send` event precedes the turn's events; later sends are refused until a hook confirms the turn |
| `send --reply <target> <text>` | live session, nothing unconfirmed, a proven caller session | delivered now, asking the receiver to answer with `tmag reply`; its `Send` event precedes what follows |
| `interrupt <target>` | session working or blocked | the interrupt is delivered once; its `Halt` event precedes what follows |
| `answer <target> <request> allow\|deny [--message M] [--answers JSON]` | the named request still open | the decision reaches that request and no other; its `Answer` event precedes what follows |
| `request <target> <request>` | the named request still open | shows what the request asks (tool and input), relayed live, never stored |

`reply` is an agent's verb: it delivers now through the asker's own channel,
or through its pane while idle. It needs a proven caller session.

All verbs share the existing refusal codes and exit codes, add
`refused:unsupported` for a verb no channel carries to the session,
`refused:idle` for an interrupt with nothing to stop, `refused:self` for an
agent's verb on its own session and `refused:agent` for an agent's answer
that `@tmag-answer` does not allow, and report `via` (`pane`, `service`,
`hook` or `inbox`) in their JSON output. `Interrupt` is already a
Codex hook name, so tmag's own event for a delivered interrupt is `Halt`.

### Drivers

| | `send` | `interrupt` | `answer` / `request` |
| --- | --- | --- | --- |
| Claude Code, terminal | person: pane while idle, inbox while working; agent: inbox, pane as fallback while idle; `--wait`: person pane, agent inbox | pane: one Esc | answer hook |
| Claude Code, background (`--bg`) | inbox: cross-session socket; a person's `--wait` is unsupported | unsupported | answer hook |
| Codex, daemon (`runtime: service`) | service: `turn/start` (joins an active turn; `--wait` needs idle) | service: `turn/interrupt` | service: join, answer the replayed request |
| Codex, `--no-daemon` (`runtime: terminal`) | pane: paste, Enter, while idle only | pane: one Esc | unsupported |
| Pi, oh-my-pi, terminal | person: pane while idle, inbox while working; agent: inbox, pane as fallback while idle; `--wait`: person pane, agent inbox | pane: one Esc | unsupported |
| Pi, oh-my-pi, serving a program | unsupported | unsupported | unsupported |

Why each cell:

- **Claude, answers: a blocking hook.** Claude shows its dialog while
  `PermissionRequest` hooks run, runs them in parallel, and applies the first
  decision; a later one is ignored. A hook that waits for `tmag answer` and
  prints nothing otherwise therefore changes nothing for the person at the
  terminal: they and the remote answerer race, and whoever answers first
  wins. The same hook runs in background sessions, so it is the only way to
  answer a session no terminal shows. `AskUserQuestion` and `ExitPlanMode`
  need the tool input echoed back with the answer; the hook has it.
- **Codex, answers: the protocol, never a hook.** Codex awaits
  `PermissionRequest` hooks before it shows its dialog, so a waiting hook
  would hold the person's dialog back. Over the protocol every client joined
  to the thread receives each request, a client that joins later gets pending
  ones replayed, the first answer wins and every client is told it was
  resolved. A short-lived `tmag answer` can join, answer, see the resolution
  and leave.
- **Codex `--no-daemon`, answers: unsupported.** Its app-server listens
  nowhere, and its dialog's keys can be pressed but cannot be bound to a
  particular request.
- **Codex daemon, send and interrupt: the protocol.** It needs no pane (a
  thread outlives its terminal), bypasses the composer, and the TUI shows the
  turn as if typed. A send without `--wait` accepts an idle or active thread:
  `turn/start` starts one or joins the running turn. `--wait` needs an idle
  thread, because only then does its prompt start a turn of its own.
- **Claude, sends: the session's own inbox.** Claude Code serves one socket
  per session and exports its path to hooks and Bash as
  `CLAUDE_CODE_MESSAGING_SOCKET`. It labels a message there as from another
  session, so it cannot approve anything or change configuration; the
  receiver's inbound controls apply, and a session that bypasses permission
  prompts holds the message for the person unless its inbound setting is
  `accept`. Background sessions bind one too, so send reaches them where
  interrupt has no terminal. An agent's prompt always takes this channel; a
  person's takes it while the session works, and stays typed while it is
  idle. tmag names the sender in Claude Code's envelope and never asserts a
  permission mode; Claude Code answers nothing on the connection, and the EOF
  after tmag closes its write half is the only proof that the frame was read.
  A send racing a session switch in the same Claude process may reach the
  session that replaced the one it named: the socket belongs to the process,
  and tmag cannot close that race.
- **Codex daemon, sends: `turn/start`.** On Codex 0.167 a `turn/start` on an
  active thread joins that turn with the same id, and on an idle thread starts
  one. A send without `--wait` takes this as it is; `--wait` requires idle so
  that its prompt starts a turn of its own. `turn/steer` adds only a
  precondition that can fail.
- **Pi and oh-my-pi, sends: the extension's socket.** The package's extension
  serves a Unix socket inside the agent and hands the text to
  `sendUserMessage`: Pi as `deliverAs: "steer"`, which starts a turn when idle
  and joins the running one when streaming; oh-my-pi as `"aside"`, which does
  the same and re-checks at each step. Pi's call is fire-and-forget, so steer
  is right in both states; oh-my-pi's steer would queue even when idle, while
  aside starts a turn then. The request names the session it is for; at
  delivery the extension reads the agent's current session from its live
  session manager and refuses it `changed` if they differ, so nothing is
  delivered. The queue stays in the agent.
- **Pane input.** It is the person's channel for an idle interactive Claude,
  Pi or oh-my-pi session, and the agent's idle fallback when no inbox can be
  proven. For Codex `--no-daemon`, which has no inbox, it is the only channel,
  while idle. Two hazards, both observed: a draft left in the composer is
  joined to the pasted prompt, and in Codex an open approval dialog swallows
  the paste while Enter approves. The idle gate rules out the dialog. No key
  clears a draft harmlessly in both agents, so tmag does not try; the README
  says so.
- **A send never takes a busy pane.** A pane takes input only while its agent
  is idle: a dialog that opens mid-turn takes the paste, and Enter approves
  it. The session's channel takes a prompt in any state: an inbox, or the
  Codex daemon's control socket. So a send to a working session goes there,
  or is refused `busy` when the session has none.
- **A person's prompt keeps the person's authority.** It is typed while the
  session is idle and takes the session's own channel while it works;
  `--wait` stays on the pane (or the Codex daemon). An agent's prompt is not
  the person's, so it takes the session's own channel in every state, with the
  pane only as an idle fallback. That keeps a person's prompt from being
  presented as a peer's and an agent's prompt from being typed as the
  person's. It does not bypass Claude Code's inbound controls: a session that
  bypasses permission prompts can hold the agent's message for the person's
  approval, as the Claude cell says.
- **Interrupt by one Esc**, only while the session is working or blocked:
  pressed twice on an empty composer, Esc opens a rewind menu in both agents,
  so after an interrupt another is refused until the agent reports again, or
  for 90 seconds. Claude reports no hook on an interrupt, and was seen not to
  send its idle notification either, so tmag records what it delivered: the
  `Halt` event reports the session idle, its prompts declined (see
  [What tmag's deliveries do](#what-tmags-deliveries-do)). Codex reports
  `Interrupt` itself.

`internal/driver` alone maps a verb, the session's agent and its `runtime`
to a channel (`pane`, `service`, `hook` or `inbox`). Send has no single
channel: the driver also takes the caller (person or agent), the response
mode and whether the session is idle, and returns the inbox first with the
pane as an idle fallback. A session lists in `capabilities` the verbs some
channel carries to it; they follow from agent and runtime only, so every
event that reports the session carries them, and clients never branch on the
agent. A pane channel still needs the attachment when the verb is used, and
is refused `unbound` without one.

### Callers

Who runs a verb is proven from the process table, never taken from the
environment alone, as attachments are:

- **An agent's command** has an agent client or service among its
  ancestors. Its session is one of those hosted by the nearest such process
  that hosts any (a sandbox helper between the tool and its agent hosts
  none): the one named by the variable the agent sets for its tools'
  commands, `CLAUDE_CODE_SESSION_ID` (Claude Code 2.1.317) or
  `CODEX_THREAD_ID` (Codex 0.166, a subagent's own thread, which maps to its
  session), or else the only one. A variable naming a session that process
  does not host proves nothing; when nothing tells, the agent is known and
  its session is not. Both variables were observed live: a Claude Code Bash
  tool and a `codex exec` shell command each ran `tmag list` and were
  identified with their session.
- **A person's command**, a script's or a remote client's has no agent
  above it. Its pane is the one whose process tree holds it outside any
  agent service, as `from` already was.

Every `Send`, `Halt` and `Answer` records the caller: `from`, its pane, and
`by`, its agent (`agent`, and `session`, `subagent` when known). Two rules
follow:

- **No session acts on itself.** An agent's `send`, `interrupt` or `answer`
  on its own session is refused `self`: the turn or request it would act on
  is the one running the command.
- **Agents answer only when allowed.** `@tmag-answer` says who may answer:
  `on` (the default) people, scripts and remote clients; `agents` agents too;
  `off` nobody, for every agent and channel. An agent's answer under `on` is
  refused `agent`. Across several tmux servers the least permissive setting
  applies.

These rules keep a cooperative agent from approving its own or a peer's
prompts by accident and make every agent decision visible in the log. They
are not a security boundary: an agent running as the user can leave the
process tree or type into a pane with tmux itself.

### Requests

A request is one prompt for the person that tmag can answer. It is a
`Pending` prompt of the session with an id tmag assigns:

Every pending prompt has a request: a hook that opens a prompt names it
with a fresh id. Requests are not events of their
own: every event that reports the session lists its open `requests`, so a
request opens and closes in the state events that report it, whoever
answered it. A request closes with its prompt.

- **Claude.** `tmag answer-hook claude` takes the place of `tmag hook claude`
  on `PermissionRequest`. Hooks on one event run in parallel, so one handler
  must both open the prompt and wait for it. It listens on a Unix socket
  under the state directory (`req/<id>.sock`) first, then applies the event
  exactly as `tmag hook` does, naming the prompt with its own id: it takes
  over the plain request of an equal prompt no hook waits on, or adds one, so
  two dialogs for equal prompts are two requests. It waits only if the
  session then names the prompt so. Connections speak one JSON object per
  line:
  - `show` returns the tool and its input, held in the hook's memory.
  - `decide` prints the decision as the hook's output, acknowledges once it
    is written, and exits.
  - `close` exits silently. `tmag hook` sends it when the adapter closes the
    prompt: the person answered in the terminal, the call ran or the turn
    ended.

  On a signal, from its start, its own timeout (590 s, inside the plugin's
  600 s) or any error it exits 0 and prints nothing, even for a connection it
  was serving when told to leave. The dialog is up throughout, so waiting
  costs nothing.
- **Codex daemon.** tmag's hook marks the prompt from `PermissionRequest`,
  which Codex fires before prompting. Its payload names no call, but every
  approval the daemon sends names one as `itemId`, the id `PreToolUse`
  reported as `tool_use_id`. So each Ask binds its request to the call it can
  only be about, the one running call of its tool by the agent that asked,
  and to none when there are several. `tmag answer` joins the thread
  (`thread/resume` with `excludeTurns`) and takes the replayed server request
  of the request's thread, turn and call, of the approval kind Codex 0.166
  maps its tool to: `Bash` and `write_stdin` to a command approval,
  `apply_patch` to a file change; other tools are `unsupported`. A request
  stays open after its approval while the call runs, and several calls of one
  turn may ask, so nothing but the call binds an approval to it; a request
  bound to no call is answered only in the agent (`ambiguous`). Approvals
  resolved during the replay are left out.
  It answers (`accept`, or `decline`, which Codex 0.166 takes although its
  approvals do not list it), waits for `serverRequest/resolved`, and leaves.
  `request` relays the replayed request's parameters.

`tmag answer` checks the request is still open under the session's lock,
logs `Answer`, and delivers:

- through the hook, it hands the decision over under the lock, and the
  request closes in the same change, which `Answer` reports;
- through the daemon, it takes the request under the lock and logs `Answer`,
  then delivers after releasing it (the daemon runs the session's hooks,
  which wait for the lock). Of two answers at once, one is delivered and the
  other refused `changed`; the request is free again once the first has given
  up, after the service timeout.

A request the person answered in the terminal a moment earlier may still
accept a decision the agent then ignores. `answer` therefore guarantees
delivery to an open request; whether the decision applied shows in the
events that follow, as a turn does for `send`.

### What tmag's deliveries do

Hooks report what the agent does, but not everything tmag's own deliveries
do: Claude reports nothing after an interrupt, and nothing when a hook's
decision closes its dialog. tmag records those effects itself, through
`session.Store.Report`, which stores the record and then logs the
delivery's event reporting it, under the session's lock like every change.
The record is the truth: a writer that dies before logging leaves the
effect in it, for the session's next event and the next snapshot to report.

`send --wait` records its prompt in the record's `Sent` whatever channel it
uses; the asynchronous modes record a prompt written to the session's own
channel, and a daemon prompt without `--wait`, in `Queued`, under the
session's lock. `Sent` clears when a turn starts; `Queued` clears on the main
agent's next report. While either stands, another send is refused `busy`. A
late idle notification for the turn before a `--wait` prompt clears neither
its `Sent` mark nor its wait; only a turn other than the one recorded before
delivery confirms it. `Sent` is tmag's bound on the turn it asked for;
`Queued` is its bound on the messages the agent takes asynchronously.

### Reaching the Codex daemon

- tmag connects straight to the daemon's control socket
  (`$CODEX_HOME/app-server-control/app-server-control.sock`) and speaks its
  WebSocket framing with a small stdlib client, for one command at a time.
  `codex app-server proxy` is not used: it adds about 30 ms and exits 0 when
  the handshake fails.
- **Proof of the channel.** The socket's peer process (`SO_PEERCRED`,
  `LOCAL_PEERPID` on macOS) must be the session's recorded host or an
  ancestor of it, since hooks may run in a helper the daemon started, and
  `thread/read` must find the thread loaded there. Otherwise the verb is
  refused `unbound`.
- **Side effects, kept small.** `send` and `interrupt` do not resume the
  thread for a client session. `answer` joins only for as long as it takes,
  sends no overrides (they would cold-resume an idle thread), and connects
  only to a daemon that already runs the session. Every call has a client
  timeout: an interrupt of a steered turn was seen never to answer.
- **Originator.** The first client to `initialize` with a non-internal name
  becomes the daemon's originator, recorded on every later thread. Checked:
  when the TUI connects first, the originator stays `codex-tui`. tmag
  connects only to a daemon that already runs the session, so it becomes the
  originator only of a daemon no TUI has connected to; that affects
  attribution only.
- **Ordering.** The daemon may run the turn's hooks, which wait for the
  session's lock, before it answers `turn/start`. So `send --wait` marks the
  session sent and logs `Send` under the lock, and makes the call after
  releasing it; the `Sent` mark keeps other senders out until the turn is
  confirmed. A send without `--wait` logs `Send` under the lock and records
  `Queued`; the daemon's answer took it into the thread's turn, and the main
  agent's next report clears the mark.

### Reaching an inbox

`send` dials the socket the hooks recorded and reads its peer. The listener
must be the process the hooks named: the nearest process above the hook that
runs the agent's program, whatever its role, which for a Claude Code
background session is its worker, not the service host it records. That
process must still run with the recorded start; otherwise the inbox is not
proven and the send is refused `unbound`, or falls back to the pane while the
session is idle and has one.

### Contract additions (`tmag/v2`, all optional fields)

- Session: `transcript` (`path` and `format`: `claude-jsonl` or
  `codex-rollout`, from the hooks' `transcript_path`; the file may not exist
  before the first turn), `capabilities`, `requests` (`id`, `kind`
  (`permission`, `question` or `plan`), `tool`, `agent` for a subagent's,
  `since`), and `subagents` (ids of running subagents).
- `list --json`: `caller`, with the caller's `pane` and `by`.
- Events: an event with `state` reports the session in full: `state`,
  `kind`, `turn`, `cwd`, `runtime`, `transcript`, `subagents`,
  `capabilities` and `requests`, and every change to any of them is such an
  event, so a client applying events to a snapshot stays current.
  `capabilities` lists `send` for every agent and runtime with a channel that
  carries it in some state: a pane, the Codex daemon, or an inbox. A Claude
  Code background session thus lists `send` and `answer`; using an inbox
  still needs the session's recorded socket. New tmag events `Halt` and
  `Answer` (with `request` and `decision`); `Send` also carries `via` `inbox`
  when it wrote the session's own channel.
  `Send`, `Halt` and `Answer` carry `from` (now on `Answer` too) and `by`
  (see [Callers](#callers)).
- Errors: `refused:unsupported`, `refused:idle`, `refused:self`,
  `refused:agent`.

Content stays where the agent writes it. A client on the same host reads the
transcript at `transcript.path`; tmag relays none of it, apart from the one
reply `read --last` prints on request
([ADR 0002](0002-agents-work-with-each-other.md)).

### Invariants, rewritten

These replace the corresponding entries in AGENTS.md:

- **tmux stays the entry point.** No daemon, no wrapper around agents, no
  second session model. Anything resident is out of scope. A tmag command may
  connect to an agent's own service for its duration and no longer.
- **Hooks fail open.** `tmag hook` always exits 0, never writes stdout (agents
  inject hook stdout into the model context), finishes within `hook.Deadline`,
  and records failures only in `<state dir>/hook.log`. `tmag answer-hook` also
  always exits 0, writes to stdout only a decision delivered by `tmag answer`,
  and on any failure, timeout, close or signal prints nothing, leaving the
  decision to the agent's own dialog.
- **No content is stored.** Hook payloads decode into `adapter.Payload`, which
  declares no prompt, message or tool-input fields. Never add one. Session
  files and event log records hold ids, states, paths and timestamps only. A
  prompt sent through an inbox is written to the agent's channel and kept
  nowhere. `tmag answer-hook` is the one reader of a tool input: it keeps it
  in memory to show it to `tmag request` and to hand it back to the agent,
  and writes it nowhere.
- **Control is gated.** `send`, `interrupt` and `answer` act only through a
  channel proven to reach the session: its one attached pane (by `process` or
  `title-id`, showing the main agent, holding its terminal's foreground), the
  agent service it runs in (a socket whose peer is the session's host or an
  ancestor of it), the answer hook waiting on the request, or an inbox whose
  listener is the agent process that named it. Each checks the state it needs
  (`send` and `send --reply` nothing unconfirmed, `send --wait` idle too,
  `interrupt` working or blocked, `answer` its request open), checks all of it
  again under the locks it needs, and logs its event before releasing them.
  `internal/driver` alone decides which channel carries a verb. `wait` refuses
  a pane that shows no session.
- **Callers are proven too.** Who runs a verb comes from process ancestry,
  distinguished by the agent's own tool variable and never named by it alone.
  People and agents use the same verbs and every control event records
  `from` and `by`; an agent never acts on its own session, and answers for
  the person only when `@tmag-answer` is `agents`.
- **Locks**, extended: sessions change only through `session.Store.Save`,
  `Report` (what a delivery of tmag's did, which the agent does not report)
  and `End`.

Answering is a default like the others: the `@tmag-answer` option (`on`,
`agents` or `off`; default `on`) says who may answer, as in
[Callers](#callers). `tmag answer` and `answer-hook` read it from the tmux
servers they find, as `@tmag-alert` is read; `answer-hook` does not wait if
any of them sets it `off`.

### Remote access

tmag serves nothing. A client that offers these verbs remotely listens on
loopback and is reached through Tailscale or Cloudflare Zero Trust, for one
person; there are no accounts and no sharing. Because a browser can be made to
send requests by any page it visits, such a client still checks the `Host`
header, refuses cross-origin requests to its actions (custom header, JSON
body, same `Origin`), and checks the identity the tunnel asserts
(`Tailscale-User-Login`, or `Cf-Access-Authenticated-User-Email`) against its
owner. Those headers are trusted only because the client listens on loopback
alone, so the tunnel is the one way in.

## Consequences

- Clients learn one set of verbs, refusals and `capabilities`. A new agent, or
  an agent release that moves a channel, changes a driver and nothing outside
  tmag.
- tmag takes on a second channel kind, the Codex daemon's protocol. It is
  marked experimental by Codex, as the `thread-id` title item already is; the
  methods used are in its stable schema, and `tmag doctor` reports when the
  daemon does not answer as expected.
- A Claude Code session that bypasses permission prompts holds a message sent
  to its inbox until its person accepts it. tmag reports the send as written;
  whether it was accepted shows in the session's next turn.
- What remains out of reach, reported as `refused:unsupported` or as state:
  - `interrupt` for Claude background sessions: no terminal. `send` reaches
    them through their inbox;
  - answers for `--no-daemon` Codex;
  - Claude's sandbox network prompts, which raise no `PermissionRequest`;
  - Codex `request_user_input` and `request_permissions`, which fire no hook,
    so tmag sees the session working;
  - Codex approvals of MCP tools, which tmag does not match to a daemon
    request.
- Pane `send` may join a draft left in the composer. For one person working
  alone this is rare, and the README says it.
- Agents do not use these verbs unprompted. Observed with Claude Code
  2.1.317 (Haiku) asked to find a peer agent in tmux and message it: with
  `tmag` only in the plugin's directory, and again with it on `PATH`, the
  agent used raw `tmux list-panes`, `capture-pane` and `send-keys`, judged
  the peer idle from its screen, left no `Send` event, and once pressed
  Enter twice. Given four lines describing `tmag list`, `send --wait`,
  `wait` and `read`, it used those three calls and was gated and recorded.
  The verbs are complete for agents; describing them to agents, and where
  the binary is, belongs to the agent plugins, as
  [ADR 0002](0002-agents-work-with-each-other.md) decides.

## Alternatives considered

- **A separate verb set or protocol for agents.** Agents calling agents is
  the A2A ground the line above excludes, and a person and an agent asking
  the same session for the same thing need the same gate. Only who may give
  consent differs, and that is one rule on one verb.
- **Trusting the agent's session variable alone.** It is set per command by
  the agent, but any process can set it; like `$TMUX_PANE`, it only narrows
  what ancestry proves.
- **Clients pick the channel per agent.** This was the previous proposal. Every
  agent release would touch tmag and every client, and each client would need
  its own gate.
- **A general protocol (ACP, A2A).** It requires launching and wrapping the
  agents and normalises their messages; see above.
- **A blocking hook for Codex too.** Codex waits for the hook before showing
  its dialog, so the person at the terminal would wait for the remote side.
- **Pane input for the Codex daemon.** It works only while a pane shows the
  thread, joins drafts, and pressing Enter at the wrong moment approves a
  dialog.
- **Clearing the composer before a paste.** No key does it harmlessly in both
  agents. Ctrl+C and a double Esc have side effects when the composer is
  empty, and Ctrl+U clears one line.
- **`tmag transcript --follow`, relaying content.** It would make tmag parse
  or pass through content it need not touch; a path suffices for a client on
  the same host. Reading the one reply that ended a turn is narrower, and
  [ADR 0002](0002-agents-work-with-each-other.md) takes it for agents.

## Known issue found on the way

Two tmag state directories on one tmux server undo each other's attachments:
each clears attachments it has no session for. A scratch `TMAG_STATE_DIR`
alone does not isolate an experiment; `TMUX_TMPDIR` must be private too, and
`TMUX` and `TMUX_PANE` unset, as AGENTS.md already says: a process that
inherits `TMUX` reaches the developer's server whatever `TMUX_TMPDIR` says. Whether pane options should record which state
directory wrote them is a separate change.

## Order of work

1. Contract: `runtime` in events, `cwd` changes, `transcript`, `subagents`.
2. The driver seam: split `query.Send` into the gate and the pane driver;
   `capabilities`; `refused:unsupported`.
3. `interrupt` with pane drivers; `Halt`.
4. The Codex service client (WebSocket, peer proof); `send` and `interrupt`
   through it.
5. Requests: `answer-hook` for Claude, `request`, `answer`, `Answer`,
   `@tmag-answer`; `Store.Report` for what deliveries do.
6. `answer` and `request` through the Codex service.
7. The example web page: requests, answers and interrupts.

Each step comes with e2e tests. The fake agent learns to answer
`PermissionRequest` like Claude and to serve a fake Codex control socket.
