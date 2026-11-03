# Codex pane identity: which channels can prove thread ↔ TUI ↔ pane

Research question: how can an external tool (tmag) prove which terminal (tmux
pane / TUI process) a Codex thread is displayed in, given that Codex 0.166 runs
hooks inside a shared app-server daemon?

Sources: `openai/codex` at tag `rust-v0.166.0` (commit `6ff1e1ca3b30`), the
official docs at `learn.chatgpt.com/docs/*` (the target of
`developers.openai.com/codex/*` redirects), and GitHub release notes
`https://github.com/openai/codex/releases/tag/rust-v0.<N>.0`. Source paths
below are relative to the repo root and line numbers are at that tag. The
locally installed CLI is `codex-cli 0.166.0`. Local observation covered process
metadata only (cmdline, environ keys, fds, socket names) and record key names
only, never content.

"Verified absent" means a targeted grep at the tag returned nothing; the grep
is named. "Not found" means it was looked for but not exhaustively ruled out.

## Summary of channels

| Channel | Mechanism | Proof strength | Needs user config | Survives thread switch in one TUI | Stability | Verdict |
|---|---|---|---|---|---|---|
| Hook process ancestry in embedded mode | `codex --no-daemon` makes the TUI host app-server in-process, so hooks are descendants of the TUI, with its env (`TMUX_PANE`) | Strong (kernel process tree) | Yes: every launch needs `--no-daemon` (no config.toml key) | Yes, since every hook of every thread runs in the TUI's tree | Flag is documented in `--help` and release notes (0.156); daemon is "experimental, lifecycle contract may change" | Strongest proof; costs the shared daemon |
| Terminal title `thread-id` item | TUI writes OSC 0 with item `thread-id`, rendered as the first 29 chars + `...` | Medium: only a prefix; any writer to the pane tty can set the title; shows the *displayed* thread (can be a subagent) | Yes: `[tui] terminal_title` must contain `thread-id` | Yes, re-rendered on every session-configured event | Item ids are internal TUI code; config key is documented, the item list is not | Best daemon-mode channel; needs prefix matching |
| Terminal title name items (`thread-name`, `thread-title`) | Name shown in title is matched against `session_index.jsonl` | Unsound: names are not unique (forks copy them) and non-name items can collide | No (`thread-name` is in the default) | Yes | Internal | Not a proof |
| Status line `thread-id` item | Full UUID in the footer, read by `tmux capture-pane` | Medium-weak (screen scraping, width-limited, spoofable by pane output) | Yes: `[tui] status_line` | Yes | Internal | Possible fallback only |
| Hook stdin payload | JSON per event | None for client identity (no client/pid/terminal field) | No | n/a | Hooks are documented | Gives `session_id` only, no pane |
| Hook / notify env | Snapshot of the daemon's env | None: daemon env, stale or missing `TMUX_PANE` | No | n/a | Documented as such in the daemon README | Dead end in daemon mode |
| Legacy `notify` | Runs in core (the daemon), argv JSON has `thread-id` and `client` name | None for pane (`client` is the name `codex-tui`, not an instance) | Yes | n/a | Legacy | Dead end in daemon mode |
| app-server JSON-RPC | `thread/loaded/list`, `thread/list`, `thread/read` | None: connection↔thread map exists only in memory, never exposed; no peer pid | No | n/a | Stable methods, versioned schema | Dead end for pane proof; has side effects |
| `ss -xp` socket pairing | TUI pid ↔ daemon accepted fd | Strong for "TUI X is connected", nothing about thread | No | n/a | OS-level | Only identifies the TUI process, not its thread |
| TUI argv | `codex resume <id-or-name>` / `codex fork <id>` | Weak: initial thread only, may be a name | No | No (/new, /resume, /fork switch in place) | CLI documented | Hint only |
| `history.jsonl` | TUI appends `{session_id, ts, text}` per prompt | Weak: no pid, file opened and closed per write | No | Partially (per prompt) | Internal | Hint only (timing correlation) |
| `logs_2.sqlite` | Rows carry `thread_id`, `process_uuid` = `pid:<pid>:<uuid>` | Weak: TUI rows locally have no `thread_id`; daemon rows carry the daemon pid | No | n/a | Internal | Not usable |
| TUI-side user commands | editor, notifications, status line, IDE IPC | None: no TUI-executed user command receives the thread id | n/a | n/a | n/a | Verified absent |

## Q1. Process model

### How the TUI picks daemon or in-process

- There are three app-server targets: `Embedded` (in-process),
  `LocalDaemon { endpoint, allow_embedded_fallback }` and `Remote`
  (`codex-rs/tui/src/lib.rs:328-337`).
- The target is chosen in `app_server_target_for_launch`
  (`codex-rs/tui/src/lib.rs:1063-1101`). With no `--remote` and no
  `CODEX_EXEC_SERVER_URL`, the TUI uses `LocalDaemon` with embedded fallback
  if the default socket probe succeeds (`codex-rs/tui/src/lib.rs:1089-1097`).
  The probe is `maybe_probe_default_daemon_socket`
  (`codex-rs/tui/src/lib.rs:542-562`, called at
  `codex-rs/tui/src/startup_orchestration.rs:363-371`).
- If no daemon answers, the TUI auto-starts one when feature
  `daemon_auto_start` is on. That feature is `Stage::Stable`,
  `default_enabled: true` (`codex-rs/features/src/lib.rs:1084-1089`); the gate
  is at `codex-rs/tui/src/startup_orchestration.rs:571-575`. Release note
  0.157.0 #47179: "Enable automatic daemon startup by default".
- These force embedded mode (`daemon_startup::exclusion`,
  `codex-rs/tui/src/daemon_startup.rs:61-88`):
  - `--no-daemon` (flag at `codex-rs/tui/src/cli.rs:87-89`, help text "Run
    without the shared background server, even if it is already running";
    release note 0.156.0 #46088)
  - `--oss`
  - workload identity
  - `CODEX_EXEC_SERVER_URL`
  - `--profile`
  - most `-c`/`--enable`/`--disable` overrides, `--strict-config`, and
    `--dangerously-bypass-hook-trust` (`config_exclusion`,
    `codex-rs/tui/src/daemon_startup.rs:90-126`)
- Implicit reuse depends only on that exclusion:
  `reuse_implicit_local_daemon = daemon_exclusion.is_none()`
  (`codex-rs/tui/src/startup_orchestration.rs:224`).
- `features.daemon_auto_start = false` only suppresses starting a daemon. A
  daemon that is already running is still reused
  (`codex-rs/tui/src/startup_orchestration.rs:363-375` versus `:571`). No
  config.toml key forces embedded mode; it needs the CLI flag or the env var.
- Other fallbacks to embedded mode:
  - CODEX_HOME on a WSL DrvFS mount
    (`codex-rs/tui/src/daemon_startup.rs:21-47`, applied at
    `codex-rs/tui/src/startup_orchestration.rs:599-605`)
  - the Bedrock sign-in wizard
    (`codex-rs/tui/src/startup_orchestration.rs:576-597`)
  - a daemon feature mismatch
  - an implicitly discovered daemon failing to initialize
    (`codex-rs/app-server-daemon/README.md:25-28`)
- In embedded mode the app-server, core and hook runtime all live in the TUI
  process (`codex-rs/app-server-client/src/lib.rs:7`).

### Locating and starting the daemon; one daemon per what

- The advertised socket is
  `$CODEX_HOME/app-server-control/app-server-control.sock`
  (`codex-rs/app-server-transport/src/transport/mod.rs:56-73`). It is a
  symlink to `/tmp/codex-daemon-<euid>/<sha256>`.
- The hash is SHA-256 of `canonicalize(parent) + file_name` of the advertised
  path (`protected_socket_path`,
  `codex-rs/app-server-transport/src/transport/unix_socket.rs:354-368`).
- The directory is fixed and "must not depend on HOME, TMPDIR, CODEX_HOME, or
  command settings" (`codex-rs/uds/src/daemon_directory.rs:1-18`).
- Clients only trust a symlink that points at the expected hash
  (`codex-rs/app-server-transport/src/transport/unix_socket.rs:325-339`).
- The key is therefore the canonical CODEX_HOME path per euid. It does not
  include config, version or tty.
  - Verified locally: sha256 of
    `/home/vscode/.codex/app-server-control/app-server-control.sock` equals the
    `ccc3ad55…` socket name.
  - The second socket, `73df…`, is the same hash of
    `~/.codex/app-server-daemon/daemon-updater.sock`.
- Launch command: `codex app-server [--remote-control] --listen unix://
  --analytics-default-enabled [--managed-daemon]`
  (`codex-rs/app-server-daemon/src/backend/pid.rs:393-420`). stdin/stdout are
  null, stderr goes to a log file, and the process calls `setsid()`
  (`codex-rs/app-server-daemon/src/backend/pid_start.rs:109-170`).
  - Local observation: the daemon has ppid 1 and is its own session leader.
- The daemon inherits the full env of the CLI process that launched it, minus
  a handoff variable. CODEX_HOME and certificate paths are absolutised
  (`codex-rs/app-server-daemon/src/backend/pid_start.rs:110`,
  `codex-rs/app-server-daemon/src/background_command.rs:16-83`).
- Its cwd is the launcher's cwd, unless that cwd is gone
  (`codex-rs/app-server-daemon/src/background_command.rs:85-87`).
- Daemon README: "Shared clients use the environment inherited when the daemon
  started. Opening a new terminal or clearing variables there does not clear
  the running daemon's environment; per-client environment isolation is not
  provided." (`codex-rs/app-server-daemon/README.md:22-24`).
  - Local observation: the running daemon's environ has `TERM`/`TERM_PROGRAM`
    keys but no `TMUX`/`TMUX_PANE` keys. Its values reflect whichever terminal
    first started it.

### Who spawns hooks, and with what env, cwd and parent

- Hooks are spawned by the process that hosts core. In daemon mode that is the
  daemon; with `--no-daemon` it is the TUI.
- Env snapshot: `Hooks::new` runs
  `let environment = Arc::new(std::env::vars_os().collect());`
  (`codex-rs/hooks/src/registry.rs:83`). It is called during core session
  construction (`codex-rs/core/src/session/session.rs:1674`).
  - This is release note 0.149.0 #39314, "Run hooks with the captured session
    environment". The capture happens in the server process, not the client.
- `build_command` calls `env_clear()`, then replays the snapshot plus
  per-handler `env`, filtering non-inheritable names
  (`codex-rs/hooks/src/engine/command_runner.rs:478-497`).
- The shell is `$SHELL -lc <command>` (from the snapshot), falling back to
  `/bin/sh` (`codex-rs/hooks/src/engine/command_runner.rs:444-474, 499-525`).
- Hooks run in `ProcessMode::NewSession`, with no controlling tty
  (`codex-rs/hooks/src/engine/command_runner.rs:470-473`). Release notes:
  0.155.0 #43876 ("Detach Unix hook commands from the controlling terminal")
  and 0.158.0 #47610 (POSIX spawn).
- cwd is the event's cwd (`codex-rs/hooks/src/engine/command_runner.rs:280-281`).
  The doc says: "Commands run with the session `cwd` as their working
  directory." (https://learn.chatgpt.com/docs/hooks).
- stdin carries the JSON payload
  (`codex-rs/hooks/src/engine/command_runner.rs:314-320`).
- Per-handler env exists only for plugin hooks: `PLUGIN_ROOT`,
  `CLAUDE_PLUGIN_ROOT`, `PLUGIN_DATA`, `CLAUDE_PLUGIN_DATA`
  (`codex-rs/hooks/src/engine/discovery.rs:311-319`).
  - Hooks get no `CODEX_THREAD_ID` or `CODEX_SESSION_ID`. Verified absent:
    `grep -rn "THREAD_ID\|SESSION_ID" codex-rs/hooks/src`.
- Environment-scoped hooks (0.165) run on their owning remote executor instead
  (`codex-rs/hooks/src/engine/command_runner.rs:249-277`).

### Does the TUI forward its env or cwd per thread?

- cwd is forwarded. In LocalDaemon mode, `thread/start.cwd = config.cwd`
  (`codex-rs/tui/src/lib.rs:360-362, 406-412`;
  `codex-rs/tui/src/app_server_session.rs:2291, 2442-2452`).
- env is not forwarded.
  - `ThreadStartParams` has no env field
    (`codex-rs/app-server-protocol/src/protocol/v2/thread.rs:62-170`; `grep
    "pub env"` in protocol v2 matches only `command_exec.rs:90` and
    `process.rs:83`).
  - `InitializeParams` carries only `clientInfo` and `capabilities`
    (`codex-rs/app-server-protocol/src/protocol/v1.rs:29-41`).
- `TMUX_PANE` is read only inside the TUI, for its own tmux probes
  (`codex-rs/tui/src/tui/tmux.rs:35-60`,
  `codex-rs/tui/src/clipboard_copy/tmux.rs:20`,
  `codex-rs/terminal-detection/src/lib.rs:365`). Verified absent in
  app-server, core and hooks (`grep -rn TMUX_PANE`).
- Shell tool env is built from the server's own environment
  (`codex-rs/protocol/src/shell_environment.rs:69-74`). The server adds
  `CODEX_THREAD_ID` (`codex-rs/core/src/exec_env.rs:30-38`;
  `codex-rs/exec-server/src/local_process.rs:763-778`) and `CODEX_SESSION_ID`
  and `CODEX_VERSION` (`codex-rs/core/src/exec_env.rs:41-50`). In daemon mode
  the tool's `TMUX_PANE` is the daemon's (stale or absent).
- The hook payload has no terminal field (see Q2).

## Q2. Hook payloads in 0.166

### Where the field lists come from

- The generated schemas are checked in at
  `codex-rs/hooks/schema/generated/*.command.input.schema.json`, built from
  structs in `codex-rs/hooks/src/schema.rs`.
- The input structs use `#[serde(deny_unknown_fields)]`, and the schemas set
  `additionalProperties: false` (except the model-request pair). The field
  lists below are therefore complete.
- The doc lists 14 events and the shared fields `session_id`,
  `transcript_path`, `cwd`, `hook_event_name`, `model`
  (https://learn.chatgpt.com/docs/hooks).

### Fields per event

| Event | Fields |
|---|---|
| SessionStart | session_id, transcript_path, cwd, hook_event_name, model, permission_mode, source (`codex-rs/hooks/src/schema.rs:500-511`) |
| SessionEnd | session_id, transcript_path, cwd, hook_event_name, reason (always `other`) (`codex-rs/hooks/src/schema.rs:516-524`, `codex-rs/hooks/src/events/session_end.rs:24`) |
| UserPromptSubmit | session_id, turn_id, agent_id?, agent_type?, transcript_path, cwd, hook_event_name, model, permission_mode, prompt |
| PreToolUse | session_id, turn_id, agent_id?, agent_type?, transcript_path, cwd, hook_event_name, model, permission_mode, tool_name, tool_input, tool_use_id (`codex-rs/hooks/src/schema.rs:279-297`) |
| PermissionRequest | as PreToolUse, without tool_use_id |
| PostToolUse | as PreToolUse, plus tool_response |
| PreCompact / PostCompact | session_id, turn_id, agent_id?, agent_type?, transcript_path, cwd, hook_event_name, model, trigger (`manual` or `auto`) |
| Stop | session_id, turn_id, transcript_path, cwd, hook_event_name, model, permission_mode, last_assistant_message, stop_hook_active |
| SubagentStart | session_id, turn_id, agent_id, agent_type, transcript_path, cwd, hook_event_name, model, permission_mode |
| SubagentStop | as Stop, plus agent_id, agent_type, agent_transcript_path |
| Interrupt | session_id, turn_id, transcript_path, cwd, hook_event_name, model, permission_mode |
| PreModelRequest / PostModelRequest | session_id, turn_id, cwd, transcript_path, model, agent_id?, agent_type?, hook_event_name. PostModelRequest adds status, error, token_usage (`codex-rs/hooks/src/events/model_request.rs:19-30, 66-81`) |

### Field semantics

- Nothing in the payload identifies the client, connection, pid or terminal.
  Verified absent: `grep -il 'originator\|client\|connection\|"pid"\|terminal\|tty\|tmux'`
  over the generated input schemas, `codex-rs/hooks/src/schema.rs` and
  `codex-rs/hooks/src/events/*.rs` (tests excluded).
- `session_id` is the root thread id, not necessarily the thread id.
  - It comes from `sess.session_id()` at every call site
    (`codex-rs/core/src/hook_runtime.rs:160, 207, 283, 327, 432, 484, 529, 553, 595, 656, 715`).
  - `session_id()` is documented as "the identity shared by the root thread and
    all descendant threads" (`codex-rs/core/src/session/session.rs:700-703`).
    It is set to `SessionId::from(thread_id)` for root threads and inherited
    for non-root agents (`codex-rs/core/src/session/session.rs:993-999`). On
    resume it comes from the rollout's `SessionMeta`
    (`codex-rs/core/src/session/session.rs:979-987`).
  - For subagent events the subagent's own thread id is in `agent_id`
    (`codex-rs/core/src/hook_runtime.rs:1095-1097`).
  - Doc: "Subagent hooks use the parent session id."
- `turn_id` is `turn_context.sub_id`, the turn's submission id
  (`codex-rs/core/src/hook_runtime.rs:150, 485, 664`; schema comment at
  `codex-rs/hooks/src/schema.rs:281`).
- SessionStart:
  - `source` is one of `startup|resume|clear|compact|fork`
    (`codex-rs/hooks/src/events/session_start.rs:24-41`; selection at
    `codex-rs/core/src/session/session.rs:2027-2039`). `fork` was added in
    0.155.0 #44349.
  - It fires lazily at the first turn, not when the thread opens
    (`codex-rs/core/src/session/turn.rs:342`;
    `codex-rs/core/src/hook_runtime.rs:132`).
- SessionEnd fires when the thread runtime is torn down
  (`codex-rs/core/src/session/handlers.rs:293, 338`), root threads only
  (`codex-rs/core/src/hook_runtime.rs:478-481`). In daemon mode that is when
  the daemon unloads the thread, not when the TUI exits.
- Legacy `notify` is the only payload with a client field:
  `{type:"agent-turn-complete", thread-id, turn-id, cwd, client?, input-messages, last-assistant-message}`
  (`codex-rs/hooks/src/legacy_notify.rs:13-41`).
  - `client` is the thread's `app_server_client_name`, written from the
    connection's `clientInfo.name` on turn/start, resume, fork and similar
    calls, last writer wins
    (`codex-rs/app-server/src/request_processors/turn_processor.rs:562`;
    `codex-rs/app-server/src/request_processors/thread_processor.rs:1659, 2522, 4367, 4815, 5744`).
  - It is the name `codex-tui`, the same for every TUI.

## Q3. app-server JSON-RPC protocol

- Transport:
  - The Unix control socket speaks WebSocket over AF_UNIX: the server does a
    tungstenite handshake per stream
    (`codex-rs/app-server-transport/src/transport/unix_socket.rs:193-243`), and
    the client targets `ws://localhost/rpc`
    (`codex-rs/app-server-client/src/remote.rs:72, 784-795`).
  - A handshake to path `/daemon/shutdown` on a managed daemon starts shutdown
    (`codex-rs/app-server-transport/src/transport/unix_socket.rs:197-204, 249-264`).
  - The docs call WebSocket transport "experimental and unsupported"
    (https://learn.chatgpt.com/docs/app-server).
- `initialize`:
  - Params are `clientInfo{name, title?, version}` and
    `capabilities{experimentalApi, optOutNotificationMethods, …}`
    (`codex-rs/app-server-protocol/src/protocol/v1.rs:29-70`).
  - The response is `userAgent, codexHome, platformFamily, platformOs`, with no
    connection id (`codex-rs/app-server-protocol/src/protocol/v1.rs:74-84`).
  - The TUI always sends `name: "codex-tui"`, `title: None`, its version and
    `experimentalApi: true` (`codex-rs/tui/src/lib.rs:528-536`;
    `codex-rs/tui/src/app_server_connection.rs:35`). It sends no pid, tty or
    terminal.
- Thread methods:
  - `thread/list`, `thread/loaded/list`, `thread/read`, `thread/resume` and
    `thread/unsubscribe` are all ungated
    (`codex-rs/app-server-protocol/src/protocol/common.rs:561, 596, 794, 875, 880`).
  - `thread/loaded/list` returns bare ids
    (`codex-rs/app-server-protocol/src/protocol/v2/thread.rs:1689-1696`).
  - `Thread` has `status`, `cwd`, `source`, `originator` and `name`, but no
    subscriber field
    (`codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs:205-293`).
- The connection↔thread map exists only in server memory:
  `ThreadEntry.connection_ids` and `thread_ids_by_connection`
  (`codex-rs/app-server/src/thread_state.rs:345-380`). Verified absent from the
  protocol: grep of `subscriber|connectionId|peerPid|clientName|pid` in
  `protocol/v2/*.rs` and `common.rs` finds only `ThreadUnsubscribeResponse`.
- `server/diagnostics` (experimental) reports only the server's own pid and
  aggregate figures
  (`codex-rs/app-server-protocol/src/protocol/v2/diagnostics.rs:14-37`).
- Peer credentials are verified absent server-side: grep of
  `peer_cred|SO_PEERCRED|ucred|getpeereid` matches only the TUI's IDE IPC
  client (`codex-rs/tui/src/ide_context/ipc.rs:569-605`).
- Several TUIs can attach to one thread.
  - `thread/resume` of a loaded thread adds the caller as a subscriber
    (`codex-rs/app-server/src/request_processors/thread_lifecycle.rs:1155-1156`;
    `codex-rs/app-server/src/thread_state.rs:701-716`).
  - Thread notifications go to every subscriber
    (`codex-rs/app-server/src/outgoing_message.rs:213-222`).
  - `thread/started` is broadcast to every connection
    (`codex-rs/app-server/src/outgoing_message.rs:773-784`).
  - A thread with no subscribers unloads after an idle delay, 60 s by default
    (`codex-rs/app-server/src/request_processors/thread_lifecycle.rs:56-63`;
    docs).
- Side effects of a passive client:
  - `initialize` records an analytics event
    (`codex-rs/app-server/src/request_processors/initialize_processor.rs:182-187`),
    and the daemon runs with `--analytics-default-enabled` (release note
    0.161.0 #49426).
  - For non-internal client names it sets the process-global default
    originator (first caller wins) and overwrites the global
    `USER_AGENT_SUFFIX`
    (`codex-rs/app-server/src/request_processors/initialize_processor.rs:163-191`).
  - The docs say `clientInfo.name` identifies the client for the Compliance
    Logs Platform.
  - `thread/list` and `thread/read` read stored data without loading threads
    (`codex-rs/app-server/src/request_processors/thread_processor.rs:3069-3130, 5983-6050`).
  - A cold `thread/resume` loads the session and queues SessionStart `resume`
    (`codex-rs/core/src/session/session.rs:2028-2056`).
  - The server has no connection-count idle shutdown
    (`codex-rs/app-server/src/lib.rs:288-319`), so an observer does not keep
    the daemon alive.
- Daemon log spans: request spans carry `app_server.connection_id` and
  `app_server.client_name` (`codex-rs/app-server/src/app_server_tracing.rs:94-113`),
  and the log DB records span context and `thread_id`
  (`codex-rs/state/src/log_db.rs:334-351, 446-471`). The connection id is an
  internal counter with no recorded peer pid, so it does not lead to a pane.
- Stability:
  - Gated methods and fields need `experimentalApi`
    (`codex-rs/app-server-protocol/src/experimental_api.rs:29-31`).
  - The docs say generated schemas are "specific to the Codex version you
    ran", and give no versioning promise.
  - The daemon is "experimental and its lifecycle contract may change"
    (`codex-rs/app-server-daemon/README.md:3-4`).
  - `--remote-control` enables the ChatGPT cloud relay, not a local feature
    (`codex-rs/cli/src/main.rs:604-606`).

## Q4. TUI-side observables

### `[tui] terminal_title`

- Type is `Option<Vec<String>>` (`codex-rs/config/src/types.rs:974-981`).
- Defaults are `activity`, `thread-name`, `project-name`
  (`codex-rs/tui/src/chatwidget/status_surfaces.rs:28-31`). The online config
  reference still says `["spinner", "project"]`
  (https://learn.chatgpt.com/docs/config-file/config-reference); `spinner` and
  `project` are aliases.
- Item ids are defined in `codex-rs/tui/src/bottom_pane/title_setup.rs:39-100`:
  app-name, project-name (project), current-dir, activity (spinner),
  run-state (status), thread-name, thread-title (thread), git-branch,
  context-remaining, context-used, five-hour-limit, weekly-limit,
  codex-version, used-tokens, total-input-tokens, total-output-tokens,
  thread-credits, estimated-thread-cost, thread-id (session-id), fast-mode,
  daybreak, model (model-name), model-with-reasoning, reasoning,
  task-progress.
- Unknown ids only warn (`codex-rs/tui/src/chatwidget/status_surfaces.rs:142`).
- Item ids are not documented anywhere in repo docs (grep of `docs/` for
  `terminal_title` finds nothing).
- `thread-id` rendering:
  - The value is the full `thread_id.to_string()`
    (`codex-rs/tui/src/chatwidget/status_surfaces.rs:828`), passed through
    `truncate_terminal_title_part(value, 32)`
    (`codex-rs/tui/src/chatwidget/status_surfaces.rs:965-967`).
  - That function keeps `max_chars - 3` graphemes and appends `...`
    (`codex-rs/tui/src/chatwidget/status_surfaces.rs:1127-1141`).
  - A 36-char UUID therefore shows as its first 29 characters plus `...`. The
    snapshot test confirms it: `550e8400-e29b-41d4-a716-44665...`
    (`codex-rs/tui/src/chatwidget/tests/terminal_title.rs:285-320`).
  - Thread ids are UUIDv7 (`codex-rs/protocol/src/thread_id.rs:30`). The
    29-char prefix holds the 48-bit timestamp plus 46 random bits, so it is
    unique in practice but can only be prefix-matched.
- `thread-title` shows the name if there is one, otherwise the full UUID, cut
  at 48 characters (`codex-rs/tui/src/chatwidget/status_surfaces.rs:845-849, 923-931`).
  Its output is ambiguous between a name and an id.
- Assembly:
  - Items are joined with `" | "`, or `" "` next to the activity item
    (`codex-rs/tui/src/bottom_pane/title_setup.rs:191-201`).
  - The `[ ! ] Action Required | …` prefix drops only activity and run-state
    (`codex-rs/tui/src/chatwidget/action_required_title.rs:5-25`).
  - Control and bidi characters are stripped and whitespace collapses. The
    whole title is capped at 240 chars. Under GNU Screen (`STY` set),
    non-ASCII becomes `?` (`codex-rs/tui/src/terminal_title.rs:36, 74-78, 129-177`).
- Emission:
  - The sequence is plain OSC 0 terminated by BEL, with no tmux passthrough
    wrapping (`codex-rs/tui/src/terminal_title.rs:100-109`), so tmux stores it
    as the pane title.
  - Nothing is written when stdout is not a tty
    (`codex-rs/tui/src/terminal_title.rs:68`).
  - On exit the title is cleared to empty, not restored
    (`codex-rs/tui/src/app.rs:1313-1318`).
- Updates:
  - The title refreshes on every status-surface refresh, including
    session-configured after /new, /resume, /fork or an agent-thread switch
    (`codex-rs/tui/src/chatwidget/status_surfaces.rs:303-313`;
    `codex-rs/tui/src/chatwidget/session_flow.rs:213, 400-424`).
  - It follows the thread currently displayed, which may be a subagent or side
    thread (`codex-rs/tui/src/app/session_lifecycle.rs:539`;
    `codex-rs/tui/src/app/thread_routing.rs:88`). In that case the shown id is
    not the hook's `session_id`.

### Status line, other escape sequences, notifications

- Status line:
  - Defaults are `model-with-reasoning`, `current-dir`, `thread-name`
    (`codex-rs/tui/src/chatwidget.rs:471`).
  - A `thread-id` item exists
    (`codex-rs/tui/src/bottom_pane/status_line_setup.rs:139`) and renders the
    full UUID, untruncated
    (`codex-rs/tui/src/chatwidget/status_surfaces.rs:828`).
  - There is no status-line command item
    (`codex-rs/tui/src/status_surface_commands.rs:38-90` only parses
    `/statusline` arguments).
- Other OSC sequences:
  - OSC 7501 (`state=…:app=codex[:kind=…]`) and OSC 21337 (iTerm2) carry
    lifecycle state and no thread id
    (`codex-rs/tui/src/terminal_program_status.rs:225-272`).
  - OSC 9 notifications (DCS-wrapped under tmux) and BEL carry no thread id
    (`codex-rs/tui/src/notifications/osc9.rs:46-52`;
    `codex-rs/tui/src/chatwidget/notifications.rs:27-34`).
  - OSC 7, OSC 133, OSC 1337 and OSC 777 are verified absent (grep over
    `codex-rs/tui/src`).
- `tui.notifications`, `notification_method` and `notification_condition` run
  inside the TUI and emit only OSC 9 or BEL; they execute no command
  (`codex-rs/config/src/types.rs:725-743, 812-826`;
  `codex-rs/tui/src/tui.rs:1053-1064`).
- `notify` runs in core, which is the daemon in daemon mode (see Q2;
  `codex-rs/hooks/src/registry.rs:179-183, 407-421`).

### TUI-executed commands, argv, files, env

- No user-configurable command executed by the TUI receives the thread id
  (checked every `Command::new` in `codex-rs/tui/src`):
  - The external editor gets a random temp `.md` path and the TUI's env
    (`codex-rs/tui/src/external_editor.rs:177-229`).
  - `!` shell commands run on the server via `thread/shellCommand`
    (`codex-rs/tui/src/app_server_session.rs:118`).
  - The `[tui]` table has no command-valued field, and quick dials reject
    shell commands (`codex-rs/config/src/types.rs:856, 869+`).
- argv:
  - `codex resume [SESSION_ID]` accepts a UUID or a name
    (`codex-rs/cli/src/main.rs:356-360`). `codex fork [SESSION_ID]` takes a
    UUID (`codex-rs/cli/src/main.rs:418-422`).
  - argv is fixed for the life of the process, while /new, /resume, /fork,
    /clear and /side switch threads in place
    (`codex-rs/tui/src/slash_command.rs:33-48, 97-107`).
- Files the TUI writes:
  - `history.jsonl` with `{session_id, ts, text}`
    (`codex-rs/message-history/src/lib.rs:62-66`), once per prompt
    (`codex-rs/tui/src/app/thread_routing.rs:580-596`). It is opened and
    closed per write (`codex-rs/message-history/src/lib.rs:143-170`).
  - `logs_2.sqlite` rows with `thread_id` and
    `process_uuid = "pid:<pid>:<uuid>"`
    (`codex-rs/state/src/log_db.rs:334-351, 494-500`). Locally, TUI-target rows
    have a null `thread_id`.
  - `codex-tui.log` only when `log_dir` is set
    (`codex-rs/tui/src/startup_orchestration.rs:906-921`).
  - No per-TUI state file was found.
- Env the TUI sets for children is fixed values only (PATH, GIT_*, FORCE_COLOR
  and similar). The TUI sets no thread variable; `CODEX_THREAD_ID` exists only
  in server-spawned tool processes.

## Q5. Client-side extension points

None executes user code in the TUI process with thread context.

- `grep -rn "client_hooks\|tui_hooks\|ClientHook" codex-rs` returns nothing
  (verified absent).
- Hooks, plugin hooks and `notify` run in core, so in the daemon.
- The TUI's built-in dynamic tools are a fixed task-management set
  (`codex-rs/tui/src/dynamic_tools.rs:1`).
- The `/ide` IPC client connects out to `$CODEX_HOME/ipc/ipc.sock` and sends
  `{sourceClientId:"codex-tui", method:"ide-context", params:{workspaceRoot}}`
  with no thread id (`codex-rs/tui/src/ide_context/ipc.rs:142-189, 685-694`).
  A listener there could read the TUI pid via SO_PEERCRED but would learn no
  thread.

## Q6. `session_index.jsonl` and thread naming

- Record: `SessionIndexEntry { id, thread_name, updated_at }`
  (`codex-rs/rollout/src/session_index.rs:26-31`). Locally, every line has
  exactly those keys.
- The file is append-only, and "the most recent entry wins"
  (`codex-rs/rollout/src/session_index.rs:33-52`).
  - Writes take a cross-process lock on `session_index.jsonl.lock`
    (`codex-rs/rollout/src/session_index.rs:24, 119-155`).
  - Readers scan backwards and skip empty names
    (`codex-rs/rollout/src/session_index.rs:157-210`).
  - The only rewrite is on thread deletion
    (`codex-rs/rollout/src/session_index.rs:75-116`, called from
    `codex-rs/thread-store/src/local/delete_thread.rs:290`).
- Writer: the thread store inside the app-server process, so the daemon in
  daemon mode (`codex-rs/thread-store/src/local/update_thread_metadata.rs:157-171, 227`).
- Triggers:
  - `thread/name/set`
    (`codex-rs/app-server/src/request_processors/thread_processor.rs:1950-1979`),
    which broadcasts `thread/name/updated`.
  - Fork, which copies the source thread's name
    (`codex-rs/app-server/src/request_processors/thread_processor.rs:5750-5765`).
  - Guardian review threads
    (`codex-rs/thread-store/src/thread_metadata_sync.rs:71-73`).
  - The TUI's automatic title: after the first message of an unnamed thread,
    the TUI generates a title of at most 36 chars and calls `thread/name/set`
    (`codex-rs/tui/src/app/thread_title.rs:1-96`;
    `codex-rs/tui/src/app/thread_routing.rs:2070-2103`). Release note 0.150.0
    #40492.
  - Manual `/rename` (`codex-rs/tui/src/chatwidget/interaction.rs:480-503`).
- Names are not unique.
  - Lookup returns several candidates, newest first
    (`codex-rs/rollout/src/session_index.rs:227-285`).
  - Locally, 207 of 607 thread ids share their latest name with another id.
- For paginated threads the authoritative name lives in SQLite; the index is
  still appended (`codex-rs/thread-store/src/local/read_thread.rs:336-341, 494-506`).
- The file is internal and undocumented. Recent churn: 0.161.0 #49297
  (backward scan) and #49305 (batched name lookups).

## Q7. Stability of each channel and release churn (0.145–0.166)

- Daemon and process model, continuously reworked:
  - 0.154.0 #42405: daemon on Windows
  - 0.155.0 #44283 / #44314: thread recovery across daemon restarts
  - 0.156.0 #46088: `--no-daemon`
  - 0.157.0 #47179: auto-start on by default
  - 0.163.0 #53173: `/daemon` restart
  - 0.166.0 #57253
  - Embedded mode via `--no-daemon` is the documented escape hatch, but its
    long-term guarantee is not stated.
- Hooks:
  - 0.145.0 #33895: SessionEnd
  - 0.149.0 #39314: captured env
  - 0.150.0 #40511: Interrupt
  - 0.155.0 #43876: detached tty
  - 0.164.0 #55144: model-request hooks
  - 0.166.0 #57198 / #57203: command cwd in tool hooks
  - Payloads are documented and schema-generated. Fields were only added in
    this range.
- Terminal title and status line:
  - 0.148.0: cost items
  - 0.150.0 #39795: hostname status item
  - 0.161.0 #49861: daybreak items
  - 0.162.0 #50375: ASCII titles under GNU Screen
  - 0.164.0 #54387 / #54388: session-only `/title`, quick dial presets
  - 0.166.0 #57516: profile and provider status items
  - The item list and rendering are internal and change often. The 29-char
    truncation is pinned by a snapshot test but not documented.
- Thread naming: 0.150.0 #40492 (automatic titles), 0.156.0 #45108 (cancel
  generation after manual rename), 0.161.0 #49297 / #49305.
- Reconnection and multi-attach: 0.153.0 #41911 (TUI reconnects after a
  dropped connection), 0.163.0 #53436 (replies routed to the originating
  connection), 0.165.0 #56351 (attached thread forks).

How each channel holds up under the scenarios in question 7:

- Several TUIs on one thread: the title channel shows the same id in several
  panes, which is correct but ambiguous. Embedded-mode ancestry cannot share a
  thread across TUIs at all: each TUI has its own server, and the second one
  meets "an active writer" (0.154.0 #43253).
- Daemon restart: the title is unaffected, since it is client-side state, and
  the TUI reconnects. Embedded mode has no daemon to restart.

## Open questions / needs live verification

- No TUI was running during this research, so the TUI↔daemon socket pairing
  (`ss -xp`) and a live TUI's argv and fds were not observed.
- Whether tmux stores the OSC 0 title as `#{pane_title}` under the user's
  config (`allow-set-title`, `set-titles`), and whether other programs in the
  pane overwrite it after Codex clears it on exit.
- Exact title text when a subagent or side thread is displayed, compared with
  the hook's `session_id` / `agent_id`.
- Whether `--no-daemon` TUIs coexist cleanly with a running daemon on the same
  CODEX_HOME, including rollout writer locks when both touch one thread.
- Whether OSC 7501 passes through tmux, and whether it is visible to
  `tmux` formats (no thread id either way).
- Whether daemon `logs_2.sqlite` rows reliably pair `app_server.connection_id`
  with `thread_id`. Even if they do, no peer pid is recorded, so the pairing
  still does not identify a pane.
- The local `app-server-daemon/` directory had no `daemon.pid` file while the
  daemon was running (expected names are at
  `codex-rs/app-server-daemon/src/lib.rs:62-64`); not investigated further.
