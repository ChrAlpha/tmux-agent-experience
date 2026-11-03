# Session-centric redesign of tmag identity and state

Date: 2026-11-03. Inputs:

- third review round of `feat/v0.1` at `b136b8d`;
- [codex-pane-identity.md](codex-pane-identity.md): Codex `rust-v0.166.0`
  source, which channels can prove thread ↔ pane;
- [claude-session-model.md](claude-session-model.md): Claude Code 2.1.317
  docs, session lifecycle, hook execution, prior art.

No code changes. This note decides what to rebuild and what to verify first.

## Verdict

The review's remaining P1 is real and is not a parser bug. `titleClaim`
treats every title item except the last as a possible thread name; with a
custom `terminal_title`, any item (model, branch, hostname) equal to some
thread's name binds that thread to the wrong pane. No exclusion list closes
it, because the title carries no field semantics and thread names are not
unique (207 of 607 local threads share a name; forks copy names).

The deeper finding is that the question v0.1 asks of every hook, *"which pane
does this hook belong to?"*, has no provable answer on the default Codex path
and, since background sessions, none for some Claude sessions either. Three
review rounds patched answers to a question the hosts do not support. The
model has to change.

## Principle: pane-native surface, session-true core

The pane-centric design came from a goal that still stands: tmag is a tmux
plugin, not a new environment.

- People keep managing agents with plain tmux.
- LLMs bring deep prior knowledge of tmux.
- tmag adds only awareness and coordination, and reuses tmux for everything
  else.

What failed was making the pane the *store and source of truth* for agent
state. The pane stays the *interface*:

| Goal | How the redesign keeps it |
| --- | --- |
| Existing tmux habits are untouched | No wrapper, no launcher, no new entry point; agents start however the user starts them. Pane ids and `session:window.pane` remain the primary targets of every command. The picker is still a tree of panes. |
| Agents work well inside tmux | The display projection in pane options is a **public, read-only tmux interface**: `tmux list-panes -a -F '#{pane_id} #{@tmag_agent} #{@tmag_state}'` answers "who is blocked" with no knowledge of tmag. `tmag` adds only what tmux cannot do safely: gated `send`, `wait`, the event stream. |
| Remote is possible later | A remote client lists conversations, not panes. Codex daemon threads and Claude background sessions have no pane at all. The contract therefore carries sessions, with panes as their attachments. |

Sessions without a pane are first-class in the contract and second-class in
tmux. They appear in `tmag list` and as a count in the picker, but never
invent tmux UI. tmux surfaces (border, bell, status) show only proven
attachments, because they cannot show doubt: a bell on the wrong window
cannot carry a question mark.

## Why the fixes did not converge

Each root cause is visible in `feat/v0.1`.

1. **State lives on the pane.** The record is a set of pane options
   (`internal/registry`). A session's state can only be written once it is
   bound, so a daemon-run Codex hook that cannot bind loses its transition
   (`hook.Run` returns early for `NeedsRecord` transitions). Identity and the
   state machine are coupled: every binding gap becomes a state bug.
2. **Identity is recomputed from volatile display data.** `World.Record`
   re-runs `titleClaim` on every `list`, `watch` tick and hook. A binding
   flaps whenever the title changes. The same coupling puts `pane.title`
   (with a 100 ms spinner) into the business snapshot, so `watch` re-sends
   snapshots without any state change.
3. **Evidence of different kinds shares one ordered lattice and one "bound"
   bit.** `process` is a kernel fact. `title-id` is a claim the TUI makes.
   `session` is circular: a record written earlier proves itself later.
   `title-name` is a heuristic. All four equally unlock `send`.
4. **The hosts are moving their agents out of the pane.** Codex has run
   threads and hooks in a shared, `setsid` daemon by default since 0.157;
   the daemon receives no client env and exposes no connection ↔ thread
   mapping. Claude's supervisor runs background sessions, and one `←` moves
   a live session there. A pane-centric model assumes the agent lives in the
   pane, and that assumption is now the exception.
5. **Hook order is not causal order.** Claude runs hooks of one event in
   parallel and some events asynchronously, with no timestamp or sequence in
   the payload. The pane lock serialises writers, not causality; the call and
   prompt bookkeeping assumes `Start` precedes `Finish`.

## Facts the design rests on

| Fact | Source |
| --- | --- |
| Codex hook payloads carry `session_id` (root thread), `turn_id`, `cwd`, `agent_id`; nothing about the client, pid or terminal | codex-pane-identity Q2 |
| Codex daemon hook env is the daemon's launch-time env; per-client env isolation is not provided | codex-pane-identity Q1 |
| app-server keeps connection ↔ thread only in memory; no peer credentials; `initialize` has side effects | codex-pane-identity Q3 |
| `codex --no-daemon` hosts app-server in the TUI, so hooks descend from the TUI; most `-c` overrides also force it; no config key does | codex-pane-identity Q1 |
| `[tui] terminal_title` item `thread-id` renders the first 29 chars of a UUIDv7 + `...`, refreshed on every thread switch, following the *displayed* thread (possibly a subagent) | codex-pane-identity Q4 |
| The daemon unloads a thread once it has had no subscriber and no activity for `thread_unload_delay_secs` (default 60), and unloading runs `SessionEnd` hooks | Codex `app-server/src/request_processors/thread_lifecycle.rs:56-60`, `core/src/config/mod.rs:4143-4144`, `core/src/session/handlers.rs:338` |
| No TUI-executed user command receives the thread id | codex-pane-identity Q4/Q5 |
| Claude hooks of an interactive session descend from the `claude` process in the pane; background sessions run under a supervisor | claude-session-model §1 |
| Claude session ids change in-process (`/clear`, `/branch`, `/resume`); one id may be live in two processes | claude-session-model §4 |
| `claude agents --json` is a documented registry of live sessions with `pid`; `~/.claude/sessions/<pid>.json` (undocumented) adds `procStart` and `tmux` | claude-session-model §5 |
| Claude background sessions run under `claude daemon run`; hooks come from the worker without `TMUX_PANE`; `claude attach` and the registries record no attach client; `/bg` ends the pane's session and forks a new id | claude-session-model §8 (live) |
| Prior art proves identity only by owning the launch or reading the agent's own registry | claude-session-model §7 |

## Model

Four things, each with one owner of truth:

```
Session   (host, session_id)          truth: the agent's hooks
  state, kind, turn, since, prompts, calls
  runtime: host process (pid, start time, role = terminal | service)

Client    (server, pane, pid, start)  truth: tmux + process table, observed, never stored
  host kind, foreground

Attachment  Client ──displays──▶ Session   truth: evidence, stored, revisable
  evidence, since, generation

Event log   append-only, seq-numbered  truth: what tmag observed, in order
```

- **A session exists from its first hook, wherever that hook runs.** Its
  state machine never needs a pane. A Codex daemon session is fully tracked,
  turns, prompts and all; it is just not attached.
- **A client is what tmux and the kernel show**: a pane whose process tree
  runs an agent client. It is recomputed on every read and costs nothing to
  be wrong about, because it carries no state.
- **An attachment is keyed by the client, not the session.** A client shows
  at most one session at a time; a session may be shown by several clients
  (Codex threads shared by TUIs, Claude resumed twice). It ends when the
  client process ends (pid + start time), when the pane goes away, or when
  stronger evidence shows the client displaying something else.
- **Liveness is proven, not timed out.** The hook's nearest agent ancestor
  is the session's host process: the `claude` process, the Codex TUI in
  embedded mode, the Codex daemon, or a Claude background worker. A session
  ends on `SessionEnd` or when its host process (pid + start time) is gone;
  a daemon restart therefore ends its sessions correctly.

### Attachment evidence

| Evidence | Applies to | Nature | Unlocks `send` |
| --- | --- | --- | --- |
| `process` | Claude interactive; Codex `--no-daemon` (and `-c`-forced embedded) | Kernel: hook descends from the client's agent process | yes |
| `title-id` | Codex in daemon mode with `thread-id` in `[tui] terminal_title` | Claim by the TUI: id prefix ≥ 29 chars in an item of its own title, re-read on every check | yes, after re-verification under the client lock |

Claude background sessions have no attachment evidence, and none is
missing from this table.

- `claude agents --json` and `~/.claude/sessions/*.json` record no attach
  client.
- `claude attach` connects only to the supervisor's control socket.
- `/bg` ends the pane's session and forks a new one under the supervisor
  ([claude-session-model.md §8](claude-session-model.md)).

Such a session is tracked, unattached, through hooks from its worker.
A pane running `claude attach` is a client with no attachment.

Removed outright:

- `title-name`: unsound by construction (the P1).
- `session`: a stored attachment already persists for exactly as long as its
  client process does; it is a lifecycle, not evidence.
- `codexidx` and the title parser beyond id prefixes, along with the thread
  names they read. Names are derived from the user's first prompt, so
  dropping them also keeps prompt-derived text out of the contract.

### Codex onboarding: one guided setting

Codex in its default daemon mode gives no proof of which pane shows a thread,
and no tmux-level switch changes that. The only ways to force embedded mode
are CLI arguments and `CODEX_EXEC_SERVER_URL`, which selects a different
executor (`codex-rs/tui/src/daemon_startup.rs:61-126`). tmag therefore asks
the user once to turn on the `thread-id` title item, and never applies it
silently.

- **The user applies it through Codex itself.** `/title` ("configure which
  items appear in the terminal title", `codex-rs/tui/src/slash_command.rs:127`)
  lets the user pick items. Codex writes the choice to the user's
  `config.toml` as `[tui] terminal_title`
  (`codex-rs/tui/src/app/event_dispatch.rs:3411-3431`,
  `codex-rs/core/src/config/edit.rs:134-141`). tmag edits nothing; the
  equivalent TOML is shown for people who prefer to edit by hand.
- **Where tmag asks:**
  - the setup popup (`prefix + M-a`), right after it connects the Codex
    plugin;
  - `tmag doctor`;
  - the picker and `tmag list`, beside a Codex pane that has no attachment;
  - `SKILL.md`, where the installing agent tells the user to run `/title`
    rather than editing Codex's configuration itself.
- **How tmag knows it worked:** by what it sees, not what the config says.
  A Codex pane whose title shows a known session's id prefix is attached.
  Profiles, project config or a later `/title` may override the user file.
- **What keeps it one-time:** `tmag doctor` flags a regression when
  daemon-mode Codex sessions are active and no Codex pane shows an id, and
  when the Codex version is outside the tested range. A change in Codex
  behaviour surfaces as a diagnosis, not as silent misbinding.
- **Until the setting is on:** every Codex session is listed with correct
  state, and each Codex pane is listed as a client with no attachment.
  Bells, `prefix + A`, the pane border and `send` need an attachment. Codex
  launched with `--no-daemon` needs no setting: its hooks prove the pane by
  process.

### State machine

- The adapters keep mapping payloads to transitions; the transition set
  (Set / Ask / Start / Finish / Settle / Answer / End) and the per-call
  prompt lifecycle from `22480d2` carry over unchanged.
- Applied per session under a per-session lock, so `NeedsRecord` drops
  disappear: a session record exists from its first event. A session first
  seen mid-turn (tmag installed while it ran) starts as `unknown` and is
  corrected by its next `Set`.
- Order tolerance:
  - Transitions carrying a turn older than the session's current turn are
    logged but not applied.
  - Calls are a set with tombstones per turn, so a `Finish` processed before
    its `Start` does not leave a phantom running call.
  - `Set` remains the only transition that replaces state wholesale.

### Storage and channels

| Data | Where | Why |
| --- | --- | --- |
| Session record | `$XDG_STATE_HOME/tmag/sessions/<host>/<id>.json`, written under its lock | Survives without a pane; O(1) on the hook path |
| Attachment | Pane options `@tmag_attach_*` | Dies with the pane, which is the correct detach |
| Display projection | Pane options (`@tmag_agent`, `@tmag_session`, `@tmag_state`, `@tmag_kind`, `@tmag_since`, `@tmag-pane-status`) | Written from session + attachment. tmux formats, `list-panes -F` and scripts read it without running tmag. Part of the versioned contract. |
| Events | Existing event log | Unchanged mechanics |

- **Event types**: `state` (session transition), `attach`, `detach`, `end`,
  `send`.
- **Observed changes become events.** A pane closing, a client exiting or a
  title switching to another id is noticed by whichever tmag process looks
  next (hook, `list`, `watch`, status job). That process reconciles under
  the client lock: if the attachment is still the one judged stale, it
  removes it and appends `detach`. Reconciliation is idempotent, so several
  observers emit one event.
- **`watch` streams events only.** It sends one snapshot at start or resume,
  then events. Its two-second tick only runs reconciliation and never diffs
  lists. Pane titles leave the contract. Pane location (`target`, `cwd`),
  which changes on rename or move, travels as a `client` event emitted by
  reconciliation when it changes. The spinner churn is gone by construction
  rather than filtered.

### Control

`send` targets what a tmux user names: a pane (`%N`, `session:window.pane`),
resolved to its attachment. A session id or prefix also works, for scripts
and remote clients. It refuses unless all of the following hold:

- exactly one attachment;
- evidence that unlocks `send`;
- the session is idle and not awaiting a submitted prompt;
- the client holds its terminal's foreground.

Each condition is re-checked under the client lock and the session lock,
taken in that order everywhere. `wait` follows session events and needs no
attachment. It ends with `ended` on `end`, not on `detach`.

### Contract

Nothing has been released (no tags, no releases), so `tmag/v1` is defined
afresh rather than versioned:

```json
{
  "schema": "tmag/v1",
  "seq": 42,
  "sessions": [
    {
      "id": "…", "host": "codex", "state": "blocked", "kind": "permission",
      "turn": "…", "since": "…", "runtime": "service",
      "clients": [{ "server": "…", "pane": "%7", "evidence": "title-id" }]
    }
  ],
  "clients": [
    { "server": "…", "pane": "%7", "target": "work:2.0", "cwd": "…",
      "host": "codex", "session": "…" },
    { "server": "…", "pane": "%9", "target": "work:3.0", "cwd": "…",
      "host": "codex", "session": null }
  ]
}
```

## Modules

| Module | Fate |
| --- | --- |
| `proc`, `tmux`, `eventlog`, `lockfile`, `binpath`, `version`, tmux plugin, agent plugins | Keep |
| `adapter` | Keep the mapping; add turn guards and call tombstones |
| `bind` | Replace with `attach`: per-host evidence providers producing attachments from a hook or an observation |
| `registry` | Split into `session` (file store, lock) and pane-option attachment + projection |
| `hook` | Rewrite: apply to the session first, then attempt attachment, then reconcile |
| `query`, `cli` watch/list/send/wait | Rewrite over sessions, clients, attachments |
| `codexidx`, title-name parsing | Delete |
| `e2e` | Keep the harness; rewrite scenarios around sessions without panes |

## Decisions

Decided by the owner:

- **Codex daemon users get a guided one-time setting** (see Codex
  onboarding). Silent configuration is out, and so is a launcher. A
  cwd-based display hint is out as well: it is not proof, and tmux surfaces
  cannot mark it as unverified.
- **`title-id` unlocks `send`**, re-verified under the client lock right
  before delivery. It is a claim the Codex TUI makes, and any program writing
  OSC 0 to the pane could imitate it. `send` also requires the Codex TUI to
  hold the foreground, and the TUI rewrites its title on every refresh.

Settled by observation:

- **No Claude registry is attachment evidence.** `claude agents --json` is
  side-effect free, but it records no attach client, and neither do the
  session files. For interactive sessions, process ancestry already proves
  what the registry would say.

## Implementation notes from observation

- Host-process classification must recognise Claude's service tree:
  `claude daemon run`, `claude bg-pty-host` and `claude bg-spare`. A
  background worker's executable name is its version (`2.1.317`), not
  `claude`, so the comm-based match in `proc.AgentKind` misses it.
- `/bg` arrives as `SessionEnd` (`prompt_input_exit`) from the pane,
  followed by `SessionStart` (`fork`) for a new id from the supervisor. The
  model handles this with no special case: one session ends with its
  attachment, and an unattached one begins.

## Live findings that shape the implementation

Codex, from [codex-pane-identity.md](codex-pane-identity.md) "Live
observations":

- **A client can name its session before the session exists.** The title
  shows the thread id from TUI startup, but `SessionStart` waits for the
  first prompt. The attachment is formed when the session first appears and
  a client's title already shows its id.
- **Viewing a subagent keeps the attachment.** The title then shows the
  subagent's thread id, which equals the hook `agent_id` of a known
  subagent of the attached session. The client stays attached, marked as
  viewing a subagent, and `send` refuses because the TUI disables input
  there. An id that is neither the session's nor one of its subagents'
  means the client shows another thread, so the attachment ends.
- **Subagents outlive turns on both hosts.** Codex's `Stop` can precede
  `SubagentStart` and carries no task list. The session keeps the set of
  running subagents (`SubagentStart` adds, `SubagentStop` removes). An idle
  main agent with running subagents is `working/background`.
- **`PreModelRequest` is not "all calls over".** A background terminal's
  `PostToolUse` arrives after later model requests. It does mean the agent
  is not waiting on the user, so closing that agent's prompts on it stays
  correct.
- **Session end:** `SessionEnd` arrives about 60 s after the last TUI
  leaves a daemon thread, and at once with `--no-daemon`. Subagents end
  with `SubagentStop` only.

Claude: hook reordering is defended against by design (turn guards, call
tombstones). Its frequency is measured from the event log during
implementation rather than in a separate spike.

## As implemented

`feat/session-model` (`d8b10b9`) implements this design. It differs from the
text above where implementation and the live findings argued otherwise, and
it was checked against real Claude Code 2.1.317 and Codex 0.166:

- process attachment and blocking;
- `/clear` (the same pane moves to the new id);
- `/bg` (the pane's session ends, an unattached service session begins);
- `claude attach`;
- a Codex daemon thread attached by `title-id`;
- `send` into the Codex TUI;
- Codex's subagent view and `working/background` after `Stop`;
- `SessionEnd` 60 s after `/quit`.

- **No turn guard.** Claude's queued prompts give a turn's later events an
  id never announced before (v0.1 feasibility, "Queued prompts break turn
  correlation"). Dropping events whose turn differs could lose a real
  permission request. Order tolerance rests on call tombstones and on `Set`
  replacing the turn's bookkeeping. Only main-agent events move the turn,
  because Codex subagents carry their own `turn_id`; v0.1 let them overwrite
  the main turn.
- **No `submitted` state.** A state that expires by time changes without an
  event, which would bring back list diffing in `watch`. The send gate still
  refuses a session whose prompt has not been confirmed within 15 s, and the
  `Send` event reports the submission.
- **Contract names follow tmux.** The contract lists `sessions` and `panes`:
  a pane entry is an agent client, and `session`, `evidence` and `view` say
  what it shows. Sessions carry `panes`. Agents are named by `agent`, as in
  events.
- **`watch` sends the pane list, not `client` events.** A `panes` document
  follows whenever the pane list differs from the last one sent. The list
  holds no titles or other volatile fields, so it changes only when an agent
  starts or exits in a pane, a pane moves, or a pane switches view.
  Attachment changes are also events (`Attach`, `Detach`), as is `Exit`.
- **One rule set, read and written.** `attach.Links` derives, from the world
  and the live sessions, which pane shows which session. `list` and `watch`
  report it without writing. `Reconcile` stores it under the pane locks and
  records the events. A pane is therefore listed correctly even before
  anything has reconciled it.
- **Session liveness reads the process table.** A session's host process is
  recorded with its start time, and `wait` checks it directly. A session
  whose agent was killed thus ends without waiting for another tmag process
  to reconcile.
