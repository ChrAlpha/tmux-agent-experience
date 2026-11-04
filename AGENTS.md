# AGENTS.md

Guidance for coding agents working in this repository. README.md describes the
product; this file describes how to change it without breaking its promises.

## What this is

A tmux plugin that knows what coding agents in its panes are doing. Three
layers, in order of importance:

1. **The tmux plugin** (`tmag.tmux`, `scripts/`) is the product: installed with
   TPM, it keeps the `tmag` binary in its own directory and gives tmux users
   alerts, key bindings and status placeholders by default.
2. **Agent plugins** (`plugins/`) are sensors: Claude Code and Codex hooks that
   report state by calling `tmag hook`, and for Claude Code's permission
   prompts `tmag answer-hook`, which also carries a decision made with
   `tmag answer`.
3. **The `tmag` binary** is the substrate: sessions and their state, which
   panes show them, the event log and a versioned JSON contract (`tmag/v1`)
   for agents, scripts and remote clients.

## Invariants

Every change must keep these. Tests guard most of them; treat a failing one as
a design problem, not a test problem.

- **tmux stays the entry point.** No daemon, no wrapper around agents, no
  second session model. Anything resident is out of scope. A tmag command may
  connect to an agent's own service for its duration and no longer.
- **Users only ever get released binaries.** The plugin downloads the archive
  for its own version and verifies its checksum; there is no build-from-source
  fallback. To try an unreleased checkout, a developer builds `bin/tmag`
  themselves (`go build -o bin/tmag ./cmd/tmag`).
- **The plugin owns one directory.** Its binary lives in
  `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/bin/`; hooks find it through
  `internal/binpath`, whose shell command and Go resolution must agree. Removing
  the plugin directory removes tmag, apart from its state directory.
- **Defaults are visible and switchable.** Installing the plugin is consent to
  its defaults; each one has an option that turns it off.
- **Hooks fail open.** `tmag hook` always exits 0, never writes stdout (agents
  inject hook stdout into the model context), finishes within `hook.Deadline`,
  and records failures only in `<state dir>/hook.log`. `tmag answer-hook`
  also always exits 0, writes to stdout only a decision delivered by
  `tmag answer`, and on any failure, timeout, close or signal prints nothing,
  leaving the decision to the agent's own dialog, which is on screen
  meanwhile.
- **No content is stored.** Hook payloads decode into `adapter.Payload`, which
  declares no prompt, message or tool-input fields. Never add one. Session
  files and event log records hold ids, states, paths and timestamps only.
  `tmag answer-hook` is the one reader of a tool input: it keeps it in memory
  to show it to `tmag request` and to hand it back to the agent, and writes
  it nowhere.
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
  session's, and logs its event before releasing them. `internal/driver`
  alone decides which channel carries a verb. `wait` refuses a pane that
  shows no session.
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
  agent does not report) and `End`, and attachments only through
  `attach.Env`, which mark a change (`@tmag_change` on a pane) before logging
  it, so a writer that dies leaves it for the next one to log rather than
  unreported.
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
| `internal/hook` | One hook invocation: apply to the session, attach on proof, update panes, log |
| `internal/adapter` | Hook payload → state transition, per agent |
| `internal/session` | Session records, the state machine, the per-session file store and lock |
| `internal/attach` | Clients, attachments and their evidence, the `@tmag_*` pane projection, reconciliation |
| `internal/query` | list / send / wait / snapshot over sessions and panes; the control verbs' gates |
| `internal/driver` | Which channel (pane, service, hook) carries each verb for each agent and runtime |
| `internal/codexsvc` | One-command client of the Codex daemon's control socket (JSON-RPC over WebSocket) |
| `internal/answer` | The answer hook's socket protocol and the decision it prints for Claude Code |
| `internal/eventlog` | Append-only, sequence-numbered event log |
| `internal/tmux` | tmux CLI wrapper; sanitises every free-text field |
| `internal/proc` | Process table, agent clients and services, hook hosts |
| `internal/lockfile` | flock bounded by a context |
| `internal/binpath` | Where hooks look for the binary; the hook command itself |
| `internal/e2e` | End-to-end tests with real tmux and a fake agent |
| `plugins/claude`, `plugins/codex` | Agent plugins (hooks only; Claude's `PermissionRequest` runs `tmag answer-hook`) |
| `.claude-plugin/`, `.agents/plugins/` | Marketplace manifests |
| `SKILL.md` | Install and update procedure that agents follow from its raw URL |

## Commands

```sh
go build ./...
go test ./...                 # unit + e2e; e2e needs tmux >= 3.2 and skips without it
go test -race ./...
golangci-lint run             # v2, config in .golangci.yml
shellcheck tmag.tmux scripts/install.sh scripts/tmag
govulncheck ./...
claude plugin validate .      # marketplace and plugin manifests
```

## Conventions

- Go standard library only. A dependency needs a reason that outweighs one more
  thing to audit in a binary every agent hook runs.
- tmux baseline is 3.2; Linux and macOS. Read the process table through
  `internal/proc` (Linux `/proc`, otherwise `ps`).
- tmux formats: wrap free-text fields with `clean()` in `internal/tmux`.
  POSIX classes like `[[:cntrl:]]` break inside formats because tmux splits on
  `:`.
- Every operation that may run on a hook path takes a `context.Context` and
  respects its deadline.
- The version lives in `internal/version/version.go` and both plugin manifests;
  `TestManifestVersions` keeps them equal.
- Install and update steps live in two places, `SKILL.md` (for agents) and
  the README (for people). Change them together, and run every command you
  write there: both are executed verbatim by someone else.
- Commits follow Conventional Commits. Code comments and docs are in English.

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
  `tmag answer-hook` waiting beside them. A line `tmag ARGS` typed into it,
  or a `{"tool":[…]}` line given to the daemon, runs tmag as the agent's
  tool, so tmag sees an agent calling. The same binary answers `--version` and `plugin …` as
  a fake host CLI. Plugin tests lay out a TPM directory and serve a local
  release over `file://`.
- `#()` status jobs run only for attached clients; to see the status line,
  attach one in a pseudo-terminal (`script -qfc 'tmux attach' file`) and read
  what it drew.
  Address panes as `e2e:%N`: a bare `%N` means the caller's own server.
- Never let a test or experiment touch the developer's tmux server: unset
  `TMUX`/`TMUX_PANE` and point `TMUX_TMPDIR` at a temporary directory.

## Checking against real agents

Hook payloads and TUI behaviour change between agent releases. Before changing
an adapter or the Codex title rules, observe the real thing:

- Claude Code: `claude --plugin-dir ./plugins/claude --setting-sources local
  --permission-mode default` inside a private tmux server, with a freshly built
  `tmag` first on `PATH` and `TMAG_STATE_DIR` pointing at a scratch directory.
  This loads the plugin without installing it.
- Codex: `tmag` reads the `thread-id` title item of Codex 0.166 (the id's
  first 29 characters and `...`). Hooks load from a project's
  `.codex/hooks.json` once the project and its hooks are trusted; Codex writes
  both trusts into the user's `config.toml`, so remove those entries, and only
  those, afterwards. A daemon keeps the environment it started with: put
  `TMAG_STATE_DIR=… tmag hook codex` in the hook command, and expect its hooks
  not to see a tmux server under a private `TMUX_TMPDIR`. To imitate a
  daemon-run hook without Codex, start it outside every pane's process tree
  (`setsid -f sh -c 'tmag hook codex < payload'`).
- Clean up afterwards: test threads (`codex delete --force <id>`), session
  transcripts, scratch servers and directories.
