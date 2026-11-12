# AGENTS.md

Guidance for coding agents working in this repository. README.md and the
landing page in `site/` introduce the product; the GitHub Wiki holds user
guides and reference. This file describes how to change it without breaking
its promises.

## What this is

A tmux plugin that knows what coding agents in its panes are doing. Three
layers, in order of importance:

1. **The tmux plugin** (`tmag.tmux`, `scripts/`) is the product: installed with
   TPM, it keeps the `tmag` binary in its own directory and gives tmux users
   alerts, key bindings and status placeholders by default.
2. **Agent plugins** (`plugins/<host>/`), two for each of Claude Code,
   Codex, Pi and oh-my-pi. `tmag` is the sensor: hooks that report state by
   calling `tmag hook` (in Pi and oh-my-pi, which have no hooks, an
   extension that runs it), and for Claude Code's permission prompts
   `tmag answer-hook`, which also carries a decision made with `tmag answer`.
   `tmag-peers` is what an agent knows of tmag: the `peers` skill, which
   describes to the model when and how to use tmag's verbs on other agents,
   and a launcher that runs the hooks' binary.
3. **The `tmag` binary** is the substrate: sessions and their state, which
   panes show them, the event log and a versioned JSON contract (`tmag/v2`)
   for agents, scripts and remote clients.

## Invariants

Every change must keep these promises. Tests guard most of them; treat a
failing one as a design problem, not a test problem. How a package keeps a
promise is documented in the package, and changes with it.

- **tmux stays the entry point.** No daemon, no wrapper around agents,
  nothing resident outside tmux: `tmag serve` runs in a tmux window the
  person opens and ends with it. A command may connect to an agent's own
  service for its duration only.
- **Lean on tmux and the OS.** Truth lives in session records and pane
  options; tmag adds no journal, repair, admission or migration over what
  tmux, the kernel and an atomic rename give. A mechanism that guards tmag
  against itself needs a failure no smaller rule fixes.
- **Users only ever get released binaries.** The plugin installs the
  checksummed archive of its own version, never a build from source; a
  developer builds `bin/tmag` themselves (`go build -o bin/tmag ./cmd/tmag`).
- **The plugin owns one directory.** The binary lives in the plugin's
  `bin/`, found through `internal/binpath`; removing the plugin removes tmag,
  apart from its state directory.
- **Defaults are visible and switchable.** Every default has an option that
  turns it off, the agents' context included: `@tmag-peers off` makes `tmag
  setup` remove the plugin whose skill description is in every session.
- **The skill teaches the CLI as it is.** Every host ships the same `peers`
  text, naming only commands, flags, refusal codes and exit codes the binary
  has (`TestPeersSkillMatchesCLI`). What to do after a refusal or failure is
  the CLI's to say, in a `next:` line on stderr that claims no more about a
  delivery than tmag proved (`TestEveryRefusalSaysWhatNext`).
- **tmag writes who sent a prompt.** An agent's prompt gets a first line
  `query.Send` writes from the proven caller; a person's gets none, and no
  prompt or message given to tmag may start with `[tmag]`. A caller never
  names itself.
- **Hooks fail open.** `tmag hook` exits 0, never writes stdout and finishes
  within `hook.Deadline`, also through the `tmag-peers` launcher; failures go
  to `hook.log` as metadata. Pi's and oh-my-pi's extensions run it in the
  order of the events without holding up the agent; the only prompt they hand
  it is one a send wrote to their inbox. `answer-hook` exits 0 too and sets up
  within that deadline, then waits for a decision within its own hook
  timeout: it prints only a
  decision `tmag answer` delivered, and nothing on any failure, leaving the
  agent's dialog, on screen meanwhile, to decide.
- **No content is stored.** Payloads decode into `adapter.Payload`, which has
  no prompt, message or tool-input field, and tmag's extensions send none;
  records, events and alerts hold ids, states, paths and times. A prompt
  sent through an inbox is written to the agent's channel and kept nowhere.
  `answer-hook` keeps a tool input in memory only, and `read --last` prints a
  reply and keeps nothing.
- **What tmag accepts is bounded where it is read.** `adapter.Decode` bounds
  every field it keeps and records cap their lists, so state stays small
  whatever an agent sends. A new field comes with its bound.
- **State belongs to one process table.** The default state directory is the
  realm's (`proc.Realm`) under `$XDG_STATE_HOME/tmag/<layout>`, so containers
  sharing a home never end each other's sessions.
- **Records are the truth; the log tells what changed.** A change lands in
  its record or pane, then in the log, best effort; gap-free numbering lets
  a follower know what it missed and take a snapshot. Waits read records;
  hook contact is never logged alone and grants nothing.
- **A session's state never depends on a pane.** Hooks apply to the session
  wherever it runs, in daemons and background supervisors too.
- **A pane shows a session only on proof.** Process ancestry or the Codex
  `thread-id` title item, never `$TMUX_PANE`, names or directories;
  ambiguity is no attachment. Bells, borders, the picker, the status line and
  `jump` show only proven attachments; anything else is `unknown`.
- **Control is gated.** `send`, `interrupt` and `answer` act only through a
  channel proven to reach the session, after checking the state they need
  again under the locks they need; an inbox is proven when its listener is
  the agent process that named it. `send` delivers now without needing an idle
  turn, and `--wait` keeps today's idle gate. `internal/driver` alone picks
  the channel from the agent, runtime, caller and response mode.
- **Callers are proven too.** Who runs a verb comes from process ancestry
  (`query.Caller`), never from a variable alone, and every control event
  records it. An agent never acts on its own session, and answers for the
  person only when `@tmag-answer` is `agents`.
- **Locks.** A session changes under its lock, a pane under its own; whoever
  takes both takes the pane's first. Each change is decided again under the
  lock and logged before it is released.
- **The HTTP API is local and owner-authorized.** `tmag serve` exposes one
  instance through `tmag/http/v2`. Each request proves a bearer credential;
  no request field, flag or variable can claim an owner or agent identity,
  and owner authority keeps every gate.
- **Authorization survives realms.** `TMAG_AUTH_DIR` (default
  `$XDG_STATE_HOME/tmag/auth`) keeps credentials as digests and metadata;
  each is revocable, and invitations expire and redeem once.
- **Nothing edits user configuration.** Install and uninstall go through each
  host's own mechanism (`claude plugin`, `codex plugin`, `pi install`,
  `omp plugin`, TPM).
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
| `internal/attach` | Clients, attachments and their evidence, the `@tmag_*` pane projection, reconciliation |
| `internal/query` | list / send / reply / wait / snapshot over sessions and panes; the control verbs' gates |
| `internal/driver` | Which channel (pane, service, hook, inbox) carries each verb for each agent and runtime |
| `internal/codexsvc` | One-command client of the Codex daemon's control socket (JSON-RPC over WebSocket) |
| `internal/inbox` | The socket a session's agent serves for messages: dial, proof, Claude Code's envelope and the extension framing |
| `internal/answer` | The answer hook's socket, under `/tmp/tmag-<uid>`, its protocol, and the decision it prints for Claude Code |
| `internal/transcript` | A turn's reply, read backward from Claude Code's JSONL transcript, Codex's rollout or Pi's session file |
| `internal/eventlog` | Append-only, sequence-numbered event log, and tails that know when they missed events |
| `internal/tmux` | tmux CLI wrapper; sanitises every free-text field |
| `internal/proc` | Process table, agent clients and services, hook hosts |
| `internal/lockfile` | flock bounded by a context, and the fixed pools of pane and session locks |
| `internal/binpath` | Where hooks look for the binary; the hook command and the agents' launcher |
| `internal/e2e` | End-to-end tests with real tmux and a fake agent |
| `plugins/<host>/tmag` | Agent plugins: hooks for Claude Code and Codex, where Claude's `PermissionRequest` runs `tmag answer-hook`; for Pi and oh-my-pi a package whose extension runs `tmag hook` and serves the session's inbox |
| `plugins/<host>/tmag-peers` | The `peers` skill and the `tmag` launcher (`bin/` in Claude Code, which puts it on `PATH`; the skill's `scripts/` elsewhere) |
| `.claude-plugin/`, `.agents/plugins/` | Marketplace manifests |
| `SKILL.md` | Install and update procedure that agents follow from its raw URL |
| `site/` | The landing page, in English and `zh-CN/`: static files that `.github/workflows/pages.yml` publishes from each release |

## Commands

```sh
go build ./...
go test ./...                 # unit + e2e; e2e needs tmux >= 3.2 and skips without it
go test -race ./...
golangci-lint run             # v2, config in .golangci.yml
shellcheck tmag.tmux scripts/install.sh scripts/tmag plugins/claude/tmag-peers/bin/tmag plugins/*/tmag-peers/skills/peers/scripts/tmag
govulncheck ./...
claude plugin validate .      # marketplace and plugin manifests
```

## Conventions

- Go 1.25 or later is required (`sync.WaitGroup.Go`); CI and releases select
  the latest stable patched toolchain, independently of the module minimum.
- Go standard library only. A dependency needs a reason that outweighs one more
  thing to audit in a binary every agent hook runs.
- tmux baseline is 3.2; Linux and macOS. Read the process table through
  `internal/proc` (Linux `/proc`, otherwise `ps`).
- tmux formats: wrap free-text fields with `clean()` in `internal/tmux`.
  POSIX classes like `[[:cntrl:]]` break inside formats because tmux splits on
  `:`.
- Every operation that may run on a hook path takes a `context.Context` and
  respects its deadline.
- What the state directory holds is its layout (`eventlog.Layout`). A change
  to it renames the layout, which gives it a default directory of its own:
  tmag never reads or migrates another layout's state. An optional record
  field an older tmag ignores, and drops when it rewrites a record, is not a
  layout change; the next hook writes it again.
- The version lives in `internal/version/version.go` and the eight plugin
  manifests; `TestManifestVersions` keeps them equal.
- The launchers are `binpath.Launcher()`, the other copies of the skill are
  the Claude copy, and oh-my-pi's `report.ts` is Pi's; tests compare them
  byte for byte, so change the generator or the first copy and copy it
  over. The extensions run `binpath.HookCommand`, which a test checks.
- Claude Code's and Codex's plugins stay under `plugins/claude` and
  `plugins/codex`: marketplaces added by `tmag setup` check out only those
  paths. Pi and oh-my-pi install `plugins/pi` and `plugins/omp` by path from
  the tmux plugin's directory, so a package matches the binary it runs.
- Install and update steps live in `SKILL.md` (for agents) and the GitHub Wiki's
  Installation and Updates page (for people), with a quick start in README.md
  and onboarding and best practices on the landing page. Keep them consistent,
  and run every command you write there: they are executed verbatim by someone
  else. Wiki pages live in the separate `tmux-agent-experience.wiki.git`
  repository.
- The landing page is HTML, CSS and a script that only enhances it: no build
  step, and no request beyond its own files. Like the skill, it names only
  commands, flags, options and keys tmag has, shows output as tmag prints it,
  and repeats README's install prompt (`TestSiteTeachesTmagAsItIs`); its tmux
  picture follows tmux's own status line and `tmag jump`'s order.
- Commits follow Conventional Commits. Code comments and canonical docs are in
  English. Localized README, Wiki and landing pages follow the English source;
  preserve commands, flags, JSON fields and output examples (a landing page's
  code differs only in its comments), and keep language links and translated
  instructions consistent when updating their source.

## Testing

- Unit tests build worlds from `proc.NewTable` and literal `tmux.Pane`s; no
  tmux needed.
- Tests run tmag as a person from outside the test's process tree
  (`harness.tmag`), and as an agent from a fake agent's tool.
- E2E tests start private servers (`tmux -L e2e` under a temporary
  `TMUX_TMPDIR`) and run `internal/e2e/fakeagent` installed as `claude`,
  `claude.exe` (as Claude Code's npm package runs), `codex`, `pi` and `omp`,
  so process names and `pane_current_command` match real agents; as `pi` and
  `omp` it reports what tmag's extensions report. Run
  as `codex app-server …` it is a Codex daemon outside every pane that runs
  hooks for any thread and serves a control socket under `CODEX_HOME`
  (`internal/codexsvc/svctest` is its server end), replaying the approvals a
  `PermissionRequest` leaves to its clients. `FAKE_KEYS=1` makes the
  interactive fake read keys like a TUI, so Escape interrupts its turn;
  `FAKE_ANSWER=1` makes it open permission dialogs as Claude Code does, with
  `tmag answer-hook` waiting beside them. `FAKE_INBOX=1` makes it serve the
  session's inbox: Claude Code's envelope, whose sender it records, or Pi's
  and oh-my-pi's extension framing. Like the agents it turns on
  bracketed paste, so a pasted prompt is one prompt, and ends each turn with
  the reply `answered: LINE` written to the transcript its hooks name, in the
  agent's format. A line `tmag ARGS` typed into it,
  or a `{"tool":[…]}` line given to the daemon, runs tmag as the agent's
  tool, so tmag sees an agent calling. The same binary answers `--version` and `plugin …` as
  a fake host CLI. Plugin tests lay out a TPM directory and serve a local
  release over `file://`.
- `TestExtensionsReport` runs Pi's and oh-my-pi's extensions on Node.js 22.6
  or later, which strips TypeScript types (skipped without it), through each
  host's events against a fake host, and reads what they ran `tmag hook`
  with. It also sends messages to each extension's inbox, idle and working,
  and checks that each payload carries its path and that shutdown unlinks the
  socket.
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
an adapter, the Codex title rules or an inbox, observe the real thing:

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
  the check takes. A hook exports `CLAUDE_CODE_MESSAGING_SOCKET`; `tmag send`
  writes Claude Code's cross-session envelope there and reads EOF. Check that
  the socket's peer is the session's process, that an idle session starts a
  turn, and that a working one reads it at its next step.
- Codex: `tmag` reads the `thread-id` title item of Codex 0.167 (the id's
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
- Pi: `pi -e ./plugins/pi/tmag -e ./plugins/pi/tmag-peers` loads the
  packages without installing them; give it scratch `PI_CODING_AGENT_DIR`,
  `TMAG_STATE_DIR` and `TMUX_PLUGIN_MANAGER_PATH`. tmag reads the events of
  Pi 1.5.0's extensions. No credentials are needed for a turn: an extension
  in the scratch agent directory can register a provider whose
  `streamSimple` answers in process (`pi --provider NAME --model ID`).
- oh-my-pi: run its release binary (`omp`, Bun inside) with a scratch `HOME`
  (its configuration is `~/.omp`) and `omp plugin install ./plugins/omp/tmag`,
  which links the package. tmag reads oh-my-pi 18.18.6's events, which differ
  from Pi's; its subagents run the extension too. A local OpenAI-compatible
  server in `~/.omp/agent/models.yml` serves as its model. Started with
  `env -i`, keep `TMUX_TMPDIR` in its environment: its hooks would otherwise
  reconcile the developer's tmux server against the scratch state.
- Pi and oh-my-pi inboxes: the package's extension serves
  `/tmp/tmag-<uid>/inbox/<agent>-<pid>.sock`; `tmag send` reaches it with
  `sendUserMessage`, as `steer` in Pi and `aside` in oh-my-pi. Check that each
  payload names the socket and that its peer is the agent process.
- Transcripts: `internal/transcript` reads Claude Code 2.1.319's JSONL (a
  line per content block, each user entry's `promptId` the hooks'
  `prompt_id`; a peer message is a meta entry whose `origin.kind` is
  `peer`), Codex 0.167's rollout (`task_complete` with `turn_id` and
  `last_agent_message`), and Pi 1.5.0's and oh-my-pi 18.18.6's session files
  (an entry per message, a user message's entry id naming its turn). Before
  changing it, run a turn and check that `tmag read ID --last` names the turn
  `tmag list --json` reports.
- Clean up afterwards: test threads (`codex delete --force <id>`), session
  transcripts, Pi's and oh-my-pi's scratch homes, scratch servers and
  directories.
