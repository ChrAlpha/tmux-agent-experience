# AGENTS.md

Guidance for coding agents working in this repository. README.md introduces the
product; the GitHub Wiki holds user guides and reference. This file describes
how to change it without breaking its promises.

## What this is

A tmux plugin that knows what coding agents in its panes are doing. Three
layers, in order of importance:

1. **The tmux plugin** (`tmag.tmux`, `scripts/`) is the product: installed with
   TPM, it keeps the `tmag` binary in its own directory and gives tmux users
   alerts, key bindings and status placeholders by default.
2. **Agent plugins** (`plugins/<host>/`), two for each of Claude Code and
   Codex. `tmag` is the sensor: hooks that report state by calling
   `tmag hook`, and for Claude Code's permission prompts `tmag answer-hook`,
   which also carries a decision made with `tmag answer`. `tmag-peers` is
   what an agent knows of tmag: the `peers` skill, which tells the model when
   and how to use tmag's verbs on other agents, and a launcher that runs the
   hooks' binary.
3. **The `tmag` binary** is the substrate: sessions and their state, which
   panes show them, the event log and a versioned JSON contract (`tmag/v1`)
   for agents, scripts and remote clients.

## Invariants

Every change must keep these. Tests guard most of them; treat a failing one as
a design problem, not a test problem.

- **tmux stays the entry point.** No daemon, no wrapper around agents, no
  second session model, and nothing resident outside tmux: `tmag serve`, the
  one command that runs until stopped, runs in a tmux window the person opens
  and ends with it. A tmag command may connect to an agent's own service for
  its duration and no longer.
- **Users only ever get released binaries.** The plugin downloads the archive
  for its own version and verifies its checksum; there is no build-from-source
  fallback. To try an unreleased checkout, a developer builds `bin/tmag`
  themselves (`go build -o bin/tmag ./cmd/tmag`).
- **The plugin owns one directory.** Its binary lives in
  `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/bin/`; hooks and the
  `tmag-peers` launcher find it through `internal/binpath`, whose shell
  commands and Go resolution must agree. Removing the plugin directory
  removes tmag, apart from its state directory.
- **Defaults are visible and switchable.** Installing the plugin is consent to
  its defaults; each one has an option that turns it off. The agents' context
  is one of them: `tmag-peers` puts its skill's description in every session,
  and `@tmag-peers off` makes `tmag setup` remove the plugin. Neither host
  turns off one skill of a plugin reliably, so the skill has a plugin of its
  own.
- **The skill teaches the CLI as it is.** Both hosts ship the same `peers`
  text, naming only commands, flags, refusal codes and exit codes the binary
  has (`TestPeersSkillMatchesCLI`).
- **tmag writes who sent a prompt.** When an agent sends one, `query.Send`
  writes its first line from the proven caller: the agent, its session and
  how to answer (`send`, `send --reply`, `reply`). A person's prompt gets
  none, and no prompt given to tmag may start with the `[tmag]` marker, so
  a first line that does is tmag's. Never let a caller name the sender.
- **Hooks fail open.** `tmag hook` always exits 0, never writes stdout (agents
  inject hook stdout into the model context), finishes within `hook.Deadline`,
  and records failures only in `<state dir>/hook.log`. CLI entry starts the
  whole budget before discovery/input, cancels cooperatively early and installs
  a final fail-open cutoff. Reusable APIs and HTTP handlers never exit the
  process; process suspension and uninterruptible kernel operations remain
  outside a normal-scheduling deadline guarantee. `tmag answer-hook`
  also always exits 0, writes to stdout only a decision delivered by
  `tmag answer`, and on any failure, timeout, close or signal prints nothing,
  leaving the decision to the agent's own dialog, which is on screen
  meanwhile. Its short setup budget is separate from its intentional waiting
  lifetime. Owned input/output and helper processes must actually cancel and
  close; a reader goroutine abandoned behind a timeout is not containment. A hook command that reaches the `tmag-peers` launcher on `PATH`
  fails open the same way when no binary exists. A hook that alerts the
  person starts `tmag notify` detached and does not wait for it; that process
  runs `@tmag-notify` for at most `notify.Timeout` and records a failure in
  `hook.log` too. Failure logging is bounded and metadata-only. Notification
  work is bounded while preserving hook and external-command deadlines.
- **No content is stored.** Hook payloads decode into `adapter.Payload`, which
  declares no prompt, message or tool-input fields. Never add one. Session
  files and event log records hold ids, states, paths and timestamps only.
  `tmag answer-hook` is the one reader of a tool input: it keeps it in memory
  to show it to `tmag request` and to hand it back to the agent, and writes
  it nowhere. `tmag read --last` is the one reader of a transcript
  (`internal/transcript`): it prints the reply it asked for and keeps
  nothing. An alert's variables (`notify.Alert`) are ids, states, tool names,
  paths, never content: a notification may leave the
  machine.
- **State belongs to one process table.** tmag proves everything from the
  processes it sees, so the default state directory is the realm's
  (`proc.Realm`: the PID namespace and boot on Linux, the boot on macOS)
  under `$XDG_STATE_HOME/tmag`: containers sharing a home would otherwise end
  each other's sessions. `TMAG_STATE_DIR` names the directory itself.
- **Hook contact is separate from state age.** Optional `last_hook_at` is
  published through complete session events. Contact-only updates preserve
  `since`; actual state transitions still change it. Contact time never
  grants state/control authority or cleanup permission. Complete record reads
  return the first exact metadata fault; reconciliation preserves unknown
  resources and pending markers rather than treating unreadable state as absent.
- **A session's state never depends on a pane.** Hooks apply to the session
  (`internal/session`) wherever it runs: the Codex daemon and Claude's
  background supervisor run sessions no terminal owns.
- **A pane shows a session only on proof.** Attachments come from the evidence
  in `internal/attach`: process ancestry, or the Codex `thread-id` title item.
  Never trust `$TMUX_PANE`: daemons and supervisors keep a stale environment.
  Names, directories and other title text are no evidence. Ambiguity means no
  attachment.
- **tmux surfaces show only proven attachments.** Bells, the pane border, the
  picker, the status line and `jump` act on attached panes; a pane tmag cannot
  prove anything about shows `unknown`.
- **Control is gated.** `send`, `interrupt` and `answer` act only through a
  channel proven to reach the session: its one attached pane (by `process` or
  `title-id`, showing the main agent, holding its terminal's foreground), the
  agent service it runs in (a socket whose peer is the session's host or an
  ancestor of it), or the answer hook waiting on the request. Each checks the
  state it needs (`send` idle, `interrupt` working or blocked, `answer` its
  request open), checks all of it again under the pane's lock and the
  session's, admits everything it will record before delivering anything,
  and logs its event before releasing them. `internal/driver` alone decides
  which channel carries a verb. `wait` refuses a pane that shows no session.
- **Callers are proven too.** Who runs a verb comes from process ancestry
  (`query.Caller`), like an attachment: a command below an agent's process is
  that agent's, and its session is one that process hosts, told apart by the
  agent's own tool variable, never named by it alone. People and agents use
  the same verbs, and every control event records `from` and `by`. An agent
  never acts on its own session, and answers for the person only when
  `@tmag-answer` is `agents`. Tests run tmag as a person outside the test's
  process tree (`harness.tmag`), and as an agent from a fake agent's tool.
- **Locks.** A session changes only under its session lock (hooks, control
  verbs); a pane's attachment and projection only under its pane lock (hooks,
  reconciliation, control verbs through a pane). Whoever takes both takes the
  pane's first. Every change is decided again under the lock against fresh
  state, a session's read through `session.Store.Current`, which first logs
  whatever a writer that died left unlogged, and its event is appended
  before the lock is released, so the log
  orders events as they happened. Sessions change only through
  `session.Store.Save`, `Report` (what a delivery of tmag's did, which the
  agent does not report) and `End`, or a change prepared as they prepare it
  (`session.Prepare*`) and made with `Publish`, and attachments only through
  `attach.Env`, which mark a change (`@tmag_change` on a pane) before logging
  it, so a writer that dies leaves it for the next one to log rather than
  unreported. Every record and event a change publishes is admitted within
  its readers' limits (4 MiB per session record, 64 MiB per log line) before
  the first is published, and derived as the repair of a writer that died
  derives it, so a marked change always settles. A marker tmag cannot read,
  on a pane or in a record, is left as it is and reported.
- **The HTTP API is local and owner-authorized.** `tmag serve` exposes one
  instance through `tmag/http/v1`, with no UI, machine inventory or outbound
  gateway. Each data request validates an independent bearer credential.
  Only a validated `auth.Grant` establishes owner authority in the common
  query path; CLI callers still derive their identity from process ancestry.
  No public flag, environment variable or request field can claim an owner
  or agent identity. Owner authority preserves every state and delivery gate.
  `credential` in a control event identifies the admitted owner credential;
  `by` continues to mean a proven agent.
- **Authorization survives realms.** `TMAG_AUTH_DIR`, defaulting to
  `$XDG_STATE_HOME/tmag/auth`, is a deployment's persistent authorization
  store. Deployments sharing a home select separate stores. Credentials have
  equal owner authority, can be individually revoked, and persist only as
  digests plus metadata. Invitations expire and are redeemed once. A revoked
  or expired credential loses its subscriptions; accepted actions finish.
- **Nothing edits user configuration.** Install and uninstall go through each
  host's own mechanism (`claude plugin`, `codex plugin`, TPM).
- **The contract is versioned.** Changing a JSON field's meaning or removing
  one requires a new schema name; adding optional fields does not.

## Layout

| Path | Role |
| --- | --- |
| `tmag.tmux` | TPM entry: binary install, options, key bindings, placeholders |
| `scripts/install.sh` | Installs the matching released binary into `bin/`, checksum verified |
| `scripts/tmag` | Runs the plugin's binary, for key bindings and status jobs |
| `cmd/tmag` | Entry point |
| `internal/cli` | Commands, flags, output, exit codes |
| `internal/hook` | One hook invocation: apply to the session, attach on proof, update panes, alert, log |
| `internal/notify` | An alert's variables, and `@tmag-notify` run detached within a time limit |
| `internal/auth` | Persistent owner credentials, one-use invitations and revocation |
| `internal/serve` | Local HTTP/SSE contract, admission and allowlisted in-process CLI operations |
| `internal/adapter` | Hook payload → state transition, per agent |
| `internal/session` | Session records, the state machine, the per-session file store and lock |
| `internal/statefile` | Owned atomic metadata publication through fixed, creator-excluded temporaries |
| `internal/ownedfs` | Runtime pinned directory scopes, no-follow ownership checks and dedicated reserved names |
| `internal/socketbind` | Short-lived executable helper: bind within an inherited directory FD, pass socket FD, reap |
| `internal/attach` | Clients, attachments and their evidence, the `@tmag_*` pane projection, reconciliation |
| `internal/query` | list / send / wait / snapshot over sessions and panes; the control verbs' gates |
| `internal/driver` | Which channel (pane, service, hook) carries each verb for each agent and runtime |
| `internal/codexsvc` | One-command client of the Codex daemon's control socket (JSON-RPC over WebSocket) |
| `internal/answer` | The answer hook's socket protocol and the decision it prints for Claude Code |
| `internal/transcript` | A turn's reply, read backward from Claude Code's JSONL transcript or Codex's rollout |
| `internal/eventlog` | Append-only, sequence-numbered event log |
| `internal/tmux` | tmux CLI wrapper; sanitises every free-text field |
| `internal/proc` | Process table, agent clients and services, hook hosts |
| `internal/lockfile` | flock bounded by a context |
| `internal/coord` | Fixed pane/session lock pools and explicit quiescent migration |
| `internal/binpath` | Where hooks look for the binary; the hook command and the agents' launcher |
| `internal/e2e` | End-to-end tests with real tmux and a fake agent |
| `plugins/<host>/tmag` | Agent plugins for Claude Code and Codex: hooks only; Claude's `PermissionRequest` runs `tmag answer-hook` |
| `plugins/<host>/tmag-peers` | The `peers` skill and the `tmag` launcher (`bin/` in Claude Code, which puts it on `PATH`; the skill's `scripts/` in Codex) |
| `.claude-plugin/`, `.agents/plugins/` | Marketplace manifests |
| `SKILL.md` | Install and update procedure that agents follow from its raw URL |

## Commands

```sh
go build ./...
go test ./...                 # unit + e2e; e2e needs tmux >= 3.2 and skips without it
go test -race ./...
golangci-lint run             # v2, config in .golangci.yml
shellcheck tmag.tmux scripts/install.sh scripts/tmag plugins/claude/tmag-peers/bin/tmag plugins/codex/tmag-peers/skills/peers/scripts/tmag
govulncheck ./...
claude plugin validate .      # marketplace and plugin manifests
```

## Conventions

- Go 1.25 or later is required for descriptor-relative `os.Root.Rename`; CI
  and releases select the latest stable patched toolchain, independently of
  the module minimum.
- Go standard library only. A dependency needs a reason that outweighs one more
  thing to audit in a binary every agent hook runs.
- tmux baseline is 3.2; Linux and macOS. Read the process table through
  `internal/proc` (Linux `/proc`, otherwise `ps`).
- tmux formats: wrap free-text fields with `clean()` in `internal/tmux`.
  POSIX classes like `[[:cntrl:]]` break inside formats because tmux splits on
  `:`.
- Every operation that may run on a hook path takes a `context.Context` and
  respects its deadline.
- `coord` pool-v2 keeps 256 slots per class, selected by the first byte of the
  existing resource SHA-256. Its pane/session class names, hash inputs and
  stable gate/pool pathnames must remain compatible across cooperating versions.
  Request endpoint positions use separate `r-XX` locks and fixed `req/` names;
  their metadata, rather than a hash assumption, binds each request to its slot.
- Unix bind and connect must stay inside a pinned directory on Linux and macOS. The
  executable dispatches `socketbind.RunChild` before CLI setup; only that
  short-lived child changes cwd, inherits creator exclusion and passes an FD.
  Library/HTTP paths never exit or change their parent's cwd.
- The version lives in `internal/version/version.go` and the four plugin
  manifests; `TestManifestVersions` keeps them equal.
- The launchers are `binpath.Launcher()`, and the Codex copy of the skill is
  the Claude copy; tests compare them byte for byte, so change the generator
  or the Claude copy and copy it over.
- Plugins stay under `plugins/claude` and `plugins/codex`: marketplaces added
  by `tmag setup` check out only those paths.
- Install and update steps live in `SKILL.md` (for agents) and the GitHub Wiki's
  Installation and Updates page (for people), with a quick start in README.md.
  Keep them consistent, and run every command you write there: they are executed
  verbatim by someone else. Wiki pages live in the separate
  `tmux-agent-experience.wiki.git` repository.
- Commits follow Conventional Commits. Code comments and canonical docs are in
  English. Localized README and Wiki pages follow the English source; preserve
  commands, flags, JSON fields and output examples, and keep language links and
  translated instructions consistent when updating their source.

## Testing

- Unit tests build worlds from `proc.NewTable` and literal `tmux.Pane`s; no
  tmux needed.
- E2E tests start private servers (`tmux -L e2e` under a temporary
  `TMUX_TMPDIR`) and run `internal/e2e/fakeagent` installed as `claude` and
  `codex`, so process names and `pane_current_command` match real agents. Run
  as `codex app-server …` it is a Codex daemon outside every pane that runs
  hooks for any thread and serves a control socket under `CODEX_HOME`
  (`internal/codexsvc/svctest` is its server end), replaying the approvals a
  `PermissionRequest` leaves to its clients. `FAKE_KEYS=1` makes the
  interactive fake read keys like a TUI, so Escape interrupts its turn;
  `FAKE_ANSWER=1` makes it open permission dialogs as Claude Code does, with
  `tmag answer-hook` waiting beside them. Like the agents it turns on
  bracketed paste, so a pasted prompt is one prompt, and ends each turn with
  the reply `answered: LINE` written to the transcript its hooks name, in the
  agent's format. A line `tmag ARGS` typed into it,
  or a `{"tool":[…]}` line given to the daemon, runs tmag as the agent's
  tool, so tmag sees an agent calling. The same binary answers `--version` and `plugin …` as
  a fake host CLI. Plugin tests lay out a TPM directory and serve a local
  release over `file://`.
- Control scenarios also run a `follower`: it rebuilds sessions and panes
  from `tmag watch` as the contract tells a client to, and `agrees` checks
  them against `tmag list --json` at the test's checkpoints. A change that
  makes a session differ without an event fails there, not on a phone.
- `tmag serve` is tested as `prefix + M-r` runs it: `serve --open` starts it
  in a window of the private server. Its tests exercise owner admission,
  independent credential revocation and common control gates. Every test
  that manages credentials sets `TMAG_AUTH_DIR` to its own temporary directory.
- `#()` status jobs run only for attached clients; to see the status line,
  attach one in a pseudo-terminal (`script -qfc 'tmux attach' file`) and read
  what it drew.
  Address panes as `e2e:%N`: a bare `%N` means the caller's own server.
- Never let a test or experiment touch the developer's tmux server: unset
  `TMUX`/`TMUX_PANE` and point `TMUX_TMPDIR` at a temporary directory.

## Checking against real agents

Hook payloads and TUI behaviour change between agent releases. Before changing
an adapter or the Codex title rules, observe the real thing:

- Claude Code: `claude --plugin-dir ./plugins/claude/tmag --setting-sources
  local --permission-mode default` inside a private tmux server, with
  `TMAG_STATE_DIR` and `TMUX_PLUGIN_MANAGER_PATH` pointing at scratch
  directories and a freshly built `tmag` in
  `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/bin/`: hooks and the
  launcher look there before `PATH`, so an installed plugin's binary would
  run instead. This loads the plugin without installing it; add
  `--plugin-dir ./plugins/claude/tmag-peers` for the skill. Your own
  commands run below an agent's process when an agent runs them, so act as
  the person from outside its tree (`setsid -f`). An agent's shell tool may
  kill what its command leaves running, a tmux server too: keep the command
  that starts the private server running, in the background, for as long as
  the check takes.
- Codex: `tmag` reads the `thread-id` title item of Codex 0.166 (the id's
  first 29 characters and `...`). Hooks load from a project's
  `.codex/hooks.json` once the project and its hooks are trusted; Codex writes
  both trusts into the user's `config.toml`, so remove those entries, and only
  those, afterwards. A daemon keeps the environment it started with: put
  `TMAG_STATE_DIR=… tmag hook codex` in the hook command, and expect its hooks
  not to see a tmux server under a private `TMUX_TMPDIR`. To imitate a
  daemon-run hook without Codex, start it outside every pane's process tree
  (`setsid -f sh -c 'tmag hook codex < payload'`). `codex --no-daemon` runs
  hooks in its own environment, so in the private server an installed `tmag`
  plugin's hooks run the scratch build, and the skill loads from the
  project's `.agents/skills/peers`, a copy of the Codex plugin's.
- Transcripts: `internal/transcript` reads Claude Code 2.1.318's JSONL (a
  line per content block, each user entry's `promptId` the hooks'
  `prompt_id`) and Codex 0.166's rollout (`task_complete` with `turn_id` and
  `last_agent_message`). Before changing it, run a turn and check that
  `tmag read ID --last` names the turn `tmag list --json` reports.
- Clean up afterwards: test threads (`codex delete --force <id>`), session
  transcripts, scratch servers and directories.
