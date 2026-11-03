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
   report state by calling `tmag hook`.
3. **The `tmag` binary** is the substrate: binding, state, the event log and a
   versioned JSON contract (`tmag/v1`) for agents, scripts and remote clients.

## Invariants

Every change must keep these. Tests guard most of them; treat a failing one as
a design problem, not a test problem.

- **tmux stays the entry point.** No daemon, no wrapper around agents, no
  second session model. Anything resident is out of scope.
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
  and records failures only in `<state dir>/hook.log`.
- **No content is stored.** Hook payloads decode into `adapter.Payload`, which
  declares no prompt, message or tool-input fields. Never add one. Event log
  records hold ids, states and timestamps only.
- **Binding needs proof.** A session is tied to a pane only by the evidence in
  `internal/bind`. Never trust `$TMUX_PANE`: Codex runs hooks in a shared
  daemon with a stale environment. Ambiguous evidence means `unbound`.
- **Control is gated.** `send` delivers only to a bound, idle agent that holds
  its terminal's foreground, after re-verifying it under the pane lock; `wait`
  refuses unbound targets.
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
| `internal/hook` | One hook invocation: map, bind, record, log |
| `internal/adapter` | Hook payload → state transition, per agent |
| `internal/bind` | Evidence rules that place a session in a pane |
| `internal/query` | list / send / wait / snapshot over a `bind.World` |
| `internal/registry` | The pane record in `@tmag_*` options |
| `internal/eventlog` | Append-only, sequence-numbered event log |
| `internal/tmux` | tmux CLI wrapper; sanitises every free-text field |
| `internal/proc` | Process table and agent-process classification |
| `internal/codexidx` | Codex thread id ↔ name index |
| `internal/lockfile` | flock bounded by a context |
| `internal/binpath` | Where hooks look for the binary; the hook command itself |
| `internal/e2e` | End-to-end tests with real tmux and a fake agent |
| `plugins/claude`, `plugins/codex` | Agent plugins (hooks only) |
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
  `codex`, so process names and `pane_current_command` match real agents. The
  same binary answers `--version` and `plugin …` as a fake host CLI. Plugin
  tests lay out a TPM directory and serve a local release over `file://`.
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
- Codex: `tmag` reads Codex 0.166's title format (`[spinner] name | project`,
  named after the first turn). To imitate a daemon-run hook, start it outside
  every pane's process tree (`setsid -f sh -c 'tmag hook codex < payload'`).
  Running it from your own shell binds to the pane your shell lives in.
- Clean up afterwards: test threads (`codex delete --force <id>`), session
  transcripts, scratch servers and directories.
