# ADR 0001: One control surface, a driver per agent and runtime

- Status: proposed
- Date: 2026-11-03
- Evidence: [Claude Code control channels](../research/claude-control-channels.md)
  (2.1.317), [Codex control channels](../research/codex-control-channels.md)
  (0.166), both observed live.

## Context

`tmag watch` gives a client every session's state and the panes that show it.
A client that also shows what a session is doing and acts on it (answers a
permission prompt from a phone, stops a runaway turn) needs three more
things: where the session's content is, a way to answer what the agent asks,
and a way to interrupt it. Today `tmag send` is the only action, and it types
into a pane.

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

### Verbs

| Verb | Acts when | Guarantees |
| --- | --- | --- |
| `send <target> <text>` | session idle, no unconfirmed prompt | one prompt starts one turn; its `Send` event precedes the turn's events; later sends are refused until a hook confirms the turn |
| `interrupt <target>` | session working or blocked | the interrupt is delivered once; its `Halt` event precedes what follows |
| `answer <target> <request> allow\|deny [--message M] [--answers JSON]` | the named request still open | the decision reaches that request and no other; its `Answer` event precedes what follows |
| `request <target> <request>` | the named request still open | shows what the request asks (tool and input), relayed live, never stored |

All verbs share the existing refusal codes and exit codes, add
`refused:unsupported` for a verb the session's driver cannot carry, and
report `via` (`pane` or `service`) in their JSON output. `Interrupt` is
already a Codex hook name, so tmag's own event for a delivered interrupt is
`Halt`.

### Drivers

| | `send` | `interrupt` | `answer` / `request` |
| --- | --- | --- | --- |
| Claude Code, terminal | pane: paste, Enter | pane: one Esc | answer hook |
| Claude Code, background (`--bg`) | unsupported | unsupported | answer hook |
| Codex, daemon (`runtime: service`) | service: `turn/start` | service: `turn/interrupt` | service: join, answer the replayed request |
| Codex, `--no-daemon` (`runtime: terminal`) | pane: paste, Enter | pane: one Esc | unsupported |

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
  turn as if typed. The daemon does not refuse a busy thread (`turn/start`
  steers the active turn), so tmag's own idle gate stays.
- **Pane input elsewhere.** It is the only channel for interactive Claude
  sessions and `--no-daemon` Codex. Two hazards, both observed: a draft left in
  the composer is joined to the pasted prompt, and in Codex an open approval
  dialog swallows the paste while Enter approves. The idle gate rules out the
  dialog. No key clears a draft harmlessly in both agents, so tmag does not try;
  the README says so.
- **Interrupt by one Esc**, only while the session is working or blocked:
  pressed twice on an empty composer, Esc opens a rewind menu in both agents.
  Claude reports no hook on an interrupt, so its state stays `working` until
  its idle notification; tmag logs `Halt` and does not guess the state.

A driver is chosen from the session's agent and `runtime` and, for pane
drivers, its attachment. Each session lists the verbs its driver can carry
in `capabilities`, so clients never branch on the agent.

### Requests

A request is one prompt for the person that tmag can answer. It is a
`Pending` prompt of the session with an id tmag assigns:

- **Claude.** `tmag answer-hook claude` takes the place of `tmag hook claude`
  on `PermissionRequest`. Hooks on one event run in parallel, so one handler
  must both open the prompt and wait for it. It applies the event exactly as
  `tmag hook` does and, in the same change under the session's lock, gives the
  prompt a request id (logging `Request`). Then it listens on a Unix socket
  under the state directory and waits. Connections speak one JSON object per
  line:
  - `show` returns the tool and its input, held in the hook's memory.
  - `decide` prints the decision as the hook's output, acknowledges once it
    is written, and exits.
  - `close` exits silently. `tmag hook` sends it when the adapter closes the
    prompt: the person answered in the terminal, the call ran or the turn
    ended.

  On SIGTERM, its own timeout or any error it exits 0 and prints nothing. The
  hook's timeout is long (the default 600 s): the dialog is up throughout, so
  waiting costs nothing.
- **Codex daemon.** tmag's hook marks the prompt from `PermissionRequest`,
  which Codex fires before prompting. `tmag answer` joins the thread
  (`thread/resume` with `excludeTurns`), matches the replayed server request by
  turn and tool, answers it, waits for `serverRequest/resolved`, and leaves.
  `request` relays the replayed request's parameters.

`tmag answer` checks the request is still open under the session's lock,
logs `Answer`, and delivers. A request the person answered in the terminal a
moment earlier may still accept a decision the agent then ignores. `answer`
therefore guarantees delivery to an open request; whether the decision applied
shows in the events that follow, as a turn does for `send`. `Resolve` is
logged when a request closes, whoever answered it.

### Reaching the Codex daemon

- tmag connects straight to the daemon's control socket
  (`$CODEX_HOME/app-server-control/app-server-control.sock`) and speaks its
  WebSocket framing with a small stdlib client, for one command at a time.
  `codex app-server proxy` is not used: it adds about 30 ms and exits 0 when
  the handshake fails.
- **Proof of the channel.** The socket's peer process (`SO_PEERCRED`,
  `LOCAL_PEERPID` on macOS) must be the session's recorded host, and
  `thread/read` must find the thread loaded there. Otherwise the verb is
  refused.
- **Side effects, kept small.** `send` and `interrupt` do not join the
  thread. `answer` joins only for as long as it takes, sends no overrides
  (they would cold-resume an idle thread), and connects only to a daemon that
  already runs the session. Every call has a client timeout: an interrupt of a
  steered turn was seen never to answer.
- **Originator.** The first client to `initialize` with a non-internal name
  becomes the daemon's originator, recorded on every later thread. Whether
  the TUI's own name counts as internal was not checked, so tmag can become
  the originator of a daemon. That affects attribution only; it is checked
  before the service client ships.
- **Ordering.** The daemon may run the turn's hooks, which wait for the
  session's lock, before it answers `turn/start`. So `send` marks the session
  sent and logs `Send` under the lock, as today, and makes the call after
  releasing it. The `Sent` mark keeps other senders out until the turn is
  confirmed.

### Contract additions (`tmag/v1`, all optional fields)

- Session: `transcript` (`path` and `format`: `claude-jsonl` or
  `codex-rollout`, from the hooks' `transcript_path`; the file may not exist
  before the first turn), `capabilities`, `requests` (`id`, `kind`
  (`permission`, `question` or `plan`), `tool`, `agent` for a subagent's,
  `since`), and `subagents` (ids of running subagents).
- Events: `runtime`; `cwd` changes are reported like state changes; new
  tmag events `Request`, `Resolve`, `Answer`, `Halt`, each with the request or
  pane it concerns and `via` where it delivers.
- Errors: `refused:unsupported`.

Content stays where the agent writes it. A client on the same host reads the
transcript at `transcript.path`; tmag has no command that relays it.

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
  files and event log records hold ids, states, paths and timestamps only.
  `tmag answer-hook` is the one reader of a tool input: it keeps it in memory
  to show it to `tmag request` and to hand it back to the agent, and writes it
  nowhere.
- **Control is gated.** `send`, `interrupt` and `answer` act only through a
  channel proven to reach the session: its one attached pane (by `process` or
  `title-id`, showing the main agent, holding its terminal's foreground) or
  the agent service it runs in (a socket whose peer is the session's host).
  Each checks the state it needs (`send` idle, `interrupt` working or blocked,
  `answer` its request open), checks all of it again under the pane's lock
  and the session's, and logs its event before releasing them. `wait` refuses
  a pane that shows no session.

Answering is a default like the others: the `@tmag-answer` option (default
`on`) turns it off, and `tmag.tmux` mirrors it into the state directory, where
`answer-hook` checks it before waiting.

### Remote access

tmag serves nothing. A client that offers these verbs remotely listens on
loopback and is reached through Tailscale or Cloudflare Zero Trust, for one
person; there are no accounts and no sharing. Because a browser can be made to
send requests by any page it visits, such a client still checks the `Host`
header, refuses cross-origin requests to its actions (custom header, JSON
body), and checks the identity the tunnel asserts (`Tailscale-User-Login`, or
the Cloudflare Access JWT) against its owner.

## Consequences

- Clients learn one set of verbs, refusals and `capabilities`. A new agent, or
  an agent release that moves a channel, changes a driver and nothing outside
  tmag.
- tmag takes on a second channel kind, the Codex daemon's protocol. It is
  marked experimental by Codex, as the `thread-id` title item already is; the
  methods used are in its stable schema, and `tmag doctor` reports when the
  daemon does not answer as expected.
- What remains out of reach, reported as `refused:unsupported` or as state:
  - `send` and `interrupt` for Claude background sessions: no terminal, and the
    per-session inbox socket labels messages as from another session;
  - answers for `--no-daemon` Codex;
  - Claude's sandbox network prompts, which raise no `PermissionRequest`;
  - Codex `request_user_input` and `request_permissions`, which fire no hook,
    so tmag sees the session working;
  - Claude's state after an interrupt, until its idle notification.
- Pane `send` may join a draft left in the composer. For one person working
  alone this is rare, and the README says it.

## Alternatives considered

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
  the same host.

## Known issue found on the way

Two tmag state directories on one tmux server undo each other's attachments:
each clears attachments it has no session for. A scratch `TMAG_STATE_DIR`
alone does not isolate an experiment; `TMUX_TMPDIR` must be private too, as
AGENTS.md already says. Whether pane options should record which state
directory wrote them is a separate change.

## Order of work

1. Contract: `runtime` in events, `cwd` changes, `transcript`, `subagents`.
2. The driver seam: split `query.Send` into the gate and the pane driver;
   `capabilities`; `refused:unsupported`.
3. `interrupt` with pane drivers; `Halt`.
4. The Codex service client (WebSocket, peer proof); `send` and `interrupt`
   through it.
5. Requests: `answer-hook` for Claude, `request`, `answer`, `Request`,
   `Resolve`, `Answer`, `@tmag-answer`.
6. `answer` and `request` through the Codex service.
7. The example web page: requests, answers and interrupts.

Each step comes with e2e tests. The fake agent learns to answer
`PermissionRequest` like Claude and to serve a fake Codex control socket.
