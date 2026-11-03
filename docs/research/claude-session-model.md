# Claude Code session model: facts for a session-centric tmag

Date: 2026-11-03. Claude Code 2.1.317. Primary sources only; claims cite:

- **[H]** https://code.claude.com/docs/en/hooks
- **[AV]** https://code.claude.com/docs/en/agent-view
- **[S]** https://code.claude.com/docs/en/sessions
- **[E]** https://code.claude.com/docs/en/env-vars
- **[AT]** https://code.claude.com/docs/en/agent-teams
- **[RC]** https://code.claude.com/docs/en/remote-control
- **[SL]** https://code.claude.com/docs/en/statusline
- **[TC]** https://code.claude.com/docs/en/terminal-config
- **[CLI]** https://code.claude.com/docs/en/cli-reference
- **[CL]** https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md

Prior-art claims cite repository paths in the named projects. "Local" marks an
observation on this machine (key names only, never content).

## 1. Hooks do not always run in the pane's process tree

- Background sessions (`--bg`, agent view, `/background` / `/bg`, `←` on an
  empty prompt, `/fork`) run as "its own Claude Code process under the
  supervisor", a background service; the terminal only runs a client
  (`claude attach`) [AV, "The supervisor process"].
- `claude --resume <id>` and in-TUI `/resume` on a running background session
  attach to it instead of loading it locally; `/resume` also moves the current
  conversation to the background [S]. One keystroke (`←`) therefore moves a
  session's hook source out of the pane's process tree.
- Remote Control server mode (`claude remote-control`) runs many concurrent
  sessions in one process (`--capacity`, default 32) [RC]. Desktop and VS Code
  go through the Agent SDK [H, Notification]; cloud sessions set
  `CLAUDE_CODE_REMOTE=true` [H].
- Command hooks run "in their own session without a controlling terminal";
  parentage survives, `/dev/tty` does not [H, "Hook input and output"].
- Hooks inherit the parent environment minus `OTEL_*` (plus scrubbing with
  `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`) [H]. Variables Claude adds:
  `CLAUDE_PROJECT_DIR`, `CLAUDE_PLUGIN_ROOT`/`_DATA` [H];
  `CLAUDE_CODE_SESSION_ID` (equals `session_id`, updated on `/clear`),
  `CLAUDECODE=1`, `CLAUDE_CODE_CHILD_SESSION=1` [E]; `CLAUDE_JOB_DIR` in
  background sessions [E].
- The supervisor and its pre-spawned workers keep the environment of the pane
  that first started them, so a conversation backgrounded from another pane
  fires hooks with the wrong `TMUX_PANE` (agent-deck
  `cmd/agent-deck/hook_bg_worker.go`, issue #2683). Inherited `TMUX_PANE` is
  no more trustworthy for Claude than for Codex.

## 2. Payloads

- Common: `session_id`, `prompt_id` (absent before the first input),
  `transcript_path`, `cwd`, `permission_mode` (not on every event),
  `hook_event_name`; `agent_id` / `agent_type` inside a subagent or an
  in-process teammate [H].
- `PermissionRequest` carries `tool_name` but **no `tool_use_id`**;
  `PreToolUse`, `PostToolUse(Failure)` carry `tool_use_id`; `PostToolBatch`
  lists `tool_calls[]` with their ids; `PermissionDenied` (auto mode only)
  carries `tool_use_id` [H].
- `Stop` carries `background_tasks[]` and `session_crons[]` and is **not**
  fired on user interrupt; `PostToolUseFailure` is not fired when a running
  tool is cancelled [H].
- `SessionStart.source`: `startup | resume | clear | compact | fork`;
  `SessionEnd.reason`: `clear | resume | logout | prompt_input_exit | other`
  [H].
- Notification types include `permission_prompt` (~6 s delay), `idle_prompt`
  (~60 s), `elicitation_dialog`, `agent_needs_input`, `agent_completed` [H].

## 3. Ordering is not guaranteed

- All matching hooks of one event run in parallel; `PostToolUse` fires
  concurrently for parallel tool calls; `async: true` hooks do not block;
  `SessionStart` at startup, resume and `/clear` runs in the background [H].
- Payloads carry no timestamp or sequence number, and no cross-event order is
  documented. agent-deck observed an old async hook scheduled after a newer
  `Stop` (agent-deck `hook_handler.go`).

## 4. Session ids and processes are many-to-many over time

- `/clear` → new id (SessionEnd `clear`, SessionStart `clear`) [H, E].
  `--continue` / `--resume` keep the id [S]. `--fork-session` and `/branch`
  create a new id; `/branch` switches the same process to it [S]. In-TUI
  `/resume` ends one id and starts another in the same process [H].
- One process therefore hosts several ids over its life. The docs disagree on
  whether one id can be live in two processes ([S] says resuming in two
  terminals interleaves into one transcript; [AV] says two processes cannot
  write one transcript). Treat both directions as possible.

## 5. Claude's own session registry

- `claude agents --json`: "Print active sessions (interactive and background)
  as a JSON array and exit (for scripting; does not require a TTY)" (local
  `claude agents --help`). Fields per [AV]: `kind`, `pid`, `status`
  (busy / waiting / idle), `waitingFor`, `sessionId`, `state`, `cwd`. [CL]
  names tmux-resurrect and status bars as intended consumers.
- Undocumented: `~/.claude/sessions/<pid>.json`. Local keys: `pid`,
  `sessionId`, `cwd`, `startedAt`, `procStart`, `kind`
  (`interactive` / `bg`), `status`, `tmux` (e.g. `"<session>:@6.%6"`),
  `messagingSocketPath`, `name`, `updatedAt`, `statusUpdatedAt`. The `tmux`
  field appears on interactive records only. Stale files persist after exit;
  `procStart` lets a reader discard them (tmux-claude-hatch
  `scripts/agents.sh` does exactly this).

## 6. Agent teams

Experimental (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`). With
`teammateMode = tmux` each teammate is a separate Claude Code instance in its
own pane [AT]; `~/.claude/teams/session-<lead id prefix>/config.json` holds
session ids and pane ids at runtime [AT]. Only in-process teammates set
`agent_id` in hooks [H].

## 7. Prior art

| Project | Session ↔ terminal identity | Codex |
| --- | --- | --- |
| claude-squad (`session/tmux/tmux.go`) | Launches the agent into a tmux session it names; state by hashing `capture-pane` | Generic program string only |
| ccmanager (`src/services/sessionManager.ts`) | Owns a PTY per worktree; screen-scraping state detectors | String matching (`stateDetector/codex.ts`) |
| agent-deck (`internal/session/instance_cmd_build.go`, `cmd/agent-deck/codex_hooks_cmd.go`) | Launches with `AGENTDECK_INSTANCE_ID`; binds `session_id` as revisable evidence, rejects cwd mismatches; drops Claude bg-worker hooks | `notify` keyed on thread id + instance env |
| opensessions (`packages/runtime-rs/src/project_dir_session.rs`, `CONTEXT.md`) | Transcript watching; maps to a tmux *session* by unique project dir; states pane presence "lacks stable thread identity" | Rollout `session_meta.cwd` |
| tmux-claude-hatch (`scripts/agents.sh`) | No hooks: `~/.claude/sessions/*.json` → pid → tty → pane, `procStart` against `/proc` | — |
| tmux-yaas, claude-tmux-sidebar | Trust `$TMUX_PANE` in the hook | — |

Every tool that proves identity either owns the launch (wrapper / env
injection) or reads a registry the agent itself writes. None proves Codex
daemon-mode pane identity without owning the launch.

## Not verified

- Whether backgrounding or plan-accept "clear context" changes `session_id`;
  whether `/compact` keeps it (implied, not stated).
- Whether `TMUX_PANE` reaches background sessions (docs: `PATH` and provider
  variables only; agent-deck reports the leak empirically).
- Whether `claude agents --json` starts the supervisor as a side effect.
- Semantics of `~/.claude/sessions/<pid>.json` `sessionId` / `tmux` after
  `/clear` or `/resume`.
- Whether split-pane teammates' hooks carry their own `session_id`.
