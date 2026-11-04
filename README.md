# tmux-agent-experience

A tmux plugin that knows what your coding agents are doing.

Claude Code and Codex keep running in your panes exactly as before. The plugin
adds what tmux cannot see on its own — which agent session runs in which pane,
and whether it is working, waiting for you, or done — and shows it the tmux way:

- **Alerts.** When an agent starts waiting for you or finishes a turn in a pane
  you are not looking at, its window gets tmux's bell flag (`!`).
  `prefix + M-n` / `M-p` already jump between flagged windows.
- **`prefix + A`** jumps to the agent that has waited longest: blocked ones
  first, then idle ones.
- **`prefix + a`** opens a tree of the panes running agents, with their state.
- **`#{tmag_status}`** in `status-left` / `status-right` shows
  `2 blocked · 1 working`.
- **`#{E:@tmag-pane-status}`** in `pane-border-format` labels each pane
  (`claude working`, `codex blocked permission`).

Underneath, the `tmag` command gives agents, scripts and remote clients the
same state as a versioned JSON contract, and can hand an idle agent its next
prompt safely.

- tmux stays the only terminal runtime: no daemon, no wrapper, no new entry point.
- Agents report through their own plugin hooks, which fail open: a broken or
  missing `tmag` never blocks a turn.
- Everything lives in the plugin's TPM directory; nothing edits your
  configuration. Prompts and agent output are never stored.

## Requirements

tmux 3.2 or newer on Linux or macOS (amd64 or arm64),
[TPM](https://github.com/tmux-plugins/tpm), `curl` or `wget`, and Claude Code
and/or Codex.

## Install

### Let your agent do it

Paste this into Claude Code or Codex, running in a tmux pane:

> Read https://raw.githubusercontent.com/ChrAlpha/tmux-agent-experience/main/SKILL.md and follow it to install or update tmag for the AI tool I am using.

The agent asks before touching your tmux configuration, installs the plugin,
connects the agent it runs in, and verifies the result. [SKILL.md](SKILL.md) is
the same procedure, readable by people. Use the same prompt to update.

### By hand

1. Add the plugin to tmux.conf and press `prefix + I`:

   ```tmux
   set -g @plugin 'ChrAlpha/tmux-agent-experience'
   ```

   On load the plugin downloads the released `tmag` binary for its version,
   checks its SHA-256, and keeps it in its own directory,
   `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/bin/` (usually
   `~/.tmux/plugins/tmux-agent-experience/bin/`).

2. Press `prefix + M-a`. A popup shows the commands that connect Claude Code
   and Codex through their own plugin managers, runs them once you confirm, and
   checks the result.

3. Start new agent sessions in tmux panes. In Codex, approve tmag's hooks with
   `/hooks`, and run `/title` once to turn on `thread-id` (see
   [Sessions and panes](#sessions-and-panes)).

Without TPM, clone the repository and run its entry point from tmux.conf:

```sh
git clone https://github.com/ChrAlpha/tmux-agent-experience ~/.tmux/plugins/tmux-agent-experience
```

```tmux
run-shell ~/.tmux/plugins/tmux-agent-experience/tmag.tmux
```

### Update

`prefix + U` updates the plugin; it fetches the matching binary on the next
load. Then `prefix + M-a` brings the agent plugins to the same version.

### Options

| Option | Default | |
| --- | --- | --- |
| `@tmag-key-picker` | `a` | prefix key for the agent tree |
| `@tmag-key-jump` | `A` | prefix key to jump to the agent waiting longest |
| `@tmag-key-setup` | `M-a` | prefix key to connect the agents and check the installation |
| `@tmag-alert` | `blocked,finished` | which changes ring a pane's bell |
| `@tmag-answer` | `on` | let `tmag answer` decide Claude Code's permission prompts and questions, beside the agent's own dialog |
| `@tmag-release-url` | GitHub releases | where to download the binary, for mirrors |

Set a key option to `off` to leave it unbound, `@tmag-alert` to `off` for
no bells, and `@tmag-answer` to `off` to answer prompts only in the agent. Set options before TPM's `run` line.

## The `tmag` command

Agent hooks find the plugin's binary on their own. To use `tmag` from a shell
or let agents call it (agent-to-agent work, scripts), put the plugin's `bin`
directory on your `PATH`, or install the released binary separately with
`mise use -g github:ChrAlpha/tmux-agent-experience` or an archive from
[Releases](https://github.com/ChrAlpha/tmux-agent-experience/releases).

### Use

```console
$ tmag list
TARGET     PANE  AGENT   STATE                FOR  ID        CWD
work:1.0   %3    claude  blocked (permission) 12s  1787a44e  ~/src/api
work:2.0   %7    codex   idle                 4m   01a19fda  ~/src/web
work:3.0   %9    codex   unknown              -    -         ~/src/web
-          -     claude  working (background) 2m   5c2e90b1  ~/src/api

$ tmag send 01a19fda --wait "Review the diff in HEAD and list risky changes"
01a19fda idle

$ tmag interrupt 01a1          # stop the running turn
$ tmag request 1787 3f9c2a01b6d4             # what an open request asks
$ tmag answer 1787 3f9c2a01b6d4 allow        # or: deny --message "not now"
$ tmag read %3 --lines 40      # recent pane output
$ tmag wait 1787 --for idle    # block until the session is idle
$ tmag watch                   # JSON lines: a snapshot, then every change
```

Each row is a pane running an agent, followed by sessions no pane shows,
such as a Claude Code background session. A target is a pane id `%N` on the
current tmux server, `SERVER:%N` with a socket name (as for `tmux -L`) or
path, or a session id or a unique prefix of at least 4 characters.

### States

A session's state comes from its agent's hooks alone, wherever the session
runs:

| State | Meaning |
| --- | --- |
| `working` | A turn is running. Kind `background` means the main agent's turn ended while subagents or background tasks still run. |
| `blocked` | The session waits for you. Kind `permission` (tool or network approval) or `question`. It stays blocked until every open prompt, from the agent or any of its subagents, is answered. A prompt closes once no call of the tool it asked about is still running in the agent that asked, once that agent's calls settle or the agent stops, or when the main agent's turn ends; a prompt of a subagent still running outlives that turn. A prompt known only from Claude's delayed permission notification, such as a sandboxed command's network request, names neither, and the next finished tool closes it. |
| `idle` | Waiting for the next prompt. Kind `error` after a failed turn. |
| `unknown` | Reported by nothing yet. A pane shows `unknown` while its agent has not reported (startup, trust dialogs, plugin not installed) or tmag has no proof of the session it shows. |

A session ends with its agent's `SessionEnd`, or when the process it runs in
exits without one.

### Sessions and panes

A session and the panes that show it are separate. A Claude Code session
runs in its terminal's own process, and so does Codex started with
`--no-daemon`; by default Codex runs every thread in a shared daemon, and
Claude Code runs background sessions under its supervisor. Their hooks then
come from a process no terminal owns, with an environment that names no pane,
so `tmag` never trusts `$TMUX_PANE`. A pane shows a session only on evidence:

| Evidence | Proof |
| --- | --- |
| `process` | The session's hooks descend from the pane's own agent process. Claude Code, and Codex run with `--no-daemon`. |
| `title-id` | Codex: the pane's terminal title shows the thread id. |

The attachment holds while the same agent process (pid and start time) runs
in the pane and, for Codex, while its title shows the session. A Codex pane
that shows one of the session's subagents stays attached with view
`subagent`. Bells, `prefix + A`, the pane border and `send` act only on
attached panes; tmux's displays cannot show doubt, so a guess is never shown
as a fact. Thread names prove nothing: they come from prompts and repeat.

For Codex in its shared daemon, turn on the `thread-id` title item once: run
`/title` in Codex and select `thread-id`. Codex saves the choice in its own
configuration. Until then, Codex sessions are listed with their state, and
Codex panes show `unknown`. The equivalent setting, for people who edit by
hand:

```toml
[tui]
terminal_title = ["activity", "thread-name", "project-name", "thread-id"]
```

A pane that runs `claude attach` shows a background session tmag cannot
prove, and so shows `unknown`; the session is listed on its own row.

### Control

tmag carries a few verbs to a session, the same for every agent: `send` a
prompt, `interrupt` a turn, `answer` a request. Each has one gate, and which
channel carries it is tmag's business, chosen for the session's agent and
runtime:

| Session | `send` | `interrupt` | `answer` |
| --- | --- | --- | --- |
| Claude Code, terminal | pane | pane | hook |
| Claude Code, background | — | — | hook |
| Codex, daemon (`runtime: service`) | service | service | service |
| Codex, `--no-daemon` | pane | pane | — |

A session's `capabilities` list the verbs some channel carries to it; a verb
nothing carries is refused with `unsupported`. Every delivery reports its
channel as `via`, in its output and its event.

#### Sending

`tmag send` delivers a prompt to an idle session through the one pane that
shows it, by `process` or `title-id`, when that pane's agent holds its
terminal and shows the main agent. Right before typing it re-checks all of
that under the pane's lock and then the session's. A busy session is refused
rather than queued, so a turn's id always belongs to the prompt that started
it; a suspended or backgrounded agent is refused, so a prompt never reaches
the shell that took the terminal over; a Codex pane showing a subagent, which
takes no input, is refused. A session shown by several panes needs a pane
target.

Delivery is a bracketed paste followed by Enter. Control characters are
removed first, so relayed text cannot end the paste early or press keys. A
draft left in the agent's composer is joined to the prompt: neither agent has
a key that clears a draft without side effects, so tmag does not try.

Hooks update a session under the same session lock, so the turn a prompt
starts is always logged after its `Send` event. Further sends are refused
until the agent's own hook confirms the turn by starting it, including after
typing reported an error, since the prompt may have arrived all the same. `send --wait`
waits for that turn to end; input that starts no turn, such as a slash
command, releases the session after 15 seconds, and `send --wait` then
reports `"started": false`.

`wait` and `send --wait` end with error code `ended` when the session ends.

#### Through the Codex daemon

A thread in Codex's daemon takes prompts and interrupts through the daemon's
control socket (`$CODEX_HOME/app-server-control/app-server-control.sock`),
whether or not a pane shows it; its terminals show the turn as if typed.
tmag connects for one command and leaves. It acts only when the socket's
process is the one the thread's hooks run in, or an ancestor of it, and asks
the daemon first whether the thread is idle (for `send`) or running (for
`interrupt`). The daemon steers a running turn instead of refusing a prompt,
so tmag's own idle gate stays: a prompt typed in a terminal at the same
moment can still be merged into the same turn. The `Send` or `Halt` event is
logged before the daemon is asked, since the daemon runs the turn's hooks
before it answers. The first client to connect to a fresh daemon may be
recorded as the originator of its later threads; tmag connects only to a
daemon that already runs the thread.

#### Answering

While a session is blocked, each prompt it waits on is a request in its
`requests`, with an `id`, a `kind` (`permission`, `question` or `plan`), the
`tool` asked about and, for a subagent's prompt, its `agent`. `tmag request`
shows what a request asks, the tool and its input, relayed from where the
agent holds them and never stored. `tmag answer TARGET REQUEST allow|deny`
decides it; `--message` goes to the agent with a deny, and a question is
answered with `--answers '{"question text":"chosen label"}'`.

For Claude Code the plugin's `PermissionRequest` hook is `tmag answer-hook`:
it reports the prompt like every hook, then waits for `tmag answer` while
Claude's own dialog is on screen. Whoever answers first decides, in the
terminal or through tmag; when the dialog is answered in the terminal, the
hook leaves. It waits in background sessions too, so their prompts can be
answered with no terminal at all. On any failure, or after 590 seconds, it
leaves without a word and the dialog alone decides. With `@tmag-answer`
`off` it does not wait, and `answer` is refused with `unsupported`.

For a thread in Codex's daemon, `answer` joins the thread through the
daemon's control socket, under the same proof as `send`. The daemon replays
the approvals still waiting, each naming the tool call it is about, and tmag
answers the one for the request's call (a command approval for `Bash` and
`write_stdin`, a file change for `apply_patch`), then leaves once the daemon
reports it resolved. Codex's `PermissionRequest` hook does not name the call,
so a request is about the one call of its tool the agent was running when it
asked; when the agent ran several, any of them may be asking, and the request
is answered only in the agent (`ambiguous`). Approvals of other tools, such
as MCP tools, are answered only in the agent (`unsupported`). Codex shows its dialog only
after its hooks have run, so no hook waits there. A Codex decision carries no
message, and a Codex session run with `--no-daemon` can be answered only in
the agent.

`answer` checks under the session's lock that the request is still open.
Through the hook it logs `Answer` once the hook has taken the decision, and
the request closes with it: the event reports the session, working on if
the prompt was its last. Through the daemon it logs `Answer` before the
daemon is told, still blocked, and the request closes when Codex reports
what followed (the call, or the agent moving on); Codex 0.166 takes
`decline` although its approvals do not list it. Of two answers to one
request at once, one is delivered and the other refused with `changed`. A
request answered in the
terminal a moment before may still take one that the agent then ignores;
what the agent did shows in the events that follow. A sandboxed command's
network request reaches no hook, and can be answered only in the agent.

#### Interrupting

`tmag interrupt` stops a session's running turn, or declines the prompt it is
blocked on, which ends the turn too. Through a pane it presses Escape once,
under the same gate as `send` except for the state: the main agent must be
working or blocked, and an idle session is refused with `idle`. Escape twice
on an empty composer opens a menu in both agents, so after an interrupt
another is refused until the agent reports again, or for 90 seconds. Codex
then reports `Interrupt` and the session is idle. Claude Code reports
nothing, not even its idle notification, so tmag records what it delivered:
the `Halt` event reports the session idle, its prompts declined.

## Machine contract

Every JSON document carries `"schema": "tmag/v1"`.

`tmag list --json` gives every session and every pane running an agent:

```json
{
  "schema": "tmag/v1",
  "seq": 42,
  "sessions": [
    {
      "id": "1787a44e-244c-4c3c-aeea-40a74ff7ef8c",
      "agent": "claude",
      "state": "blocked",
      "kind": "permission",
      "turn": "00161c25-78f9-4c24-8799-5ed63c406153",
      "since": "2026-11-03T05:20:28Z",
      "cwd": "/home/me/src/api",
      "runtime": "terminal",
      "capabilities": ["send", "interrupt", "answer"],
      "requests": [
        { "id": "3f9c2a01b6d4", "kind": "permission", "tool": "Bash", "since": "2026-11-03T05:20:28Z" }
      ],
      "transcript": {
        "path": "/home/me/.claude/projects/-home-me-src-api/1787a44e-244c-4c3c-aeea-40a74ff7ef8c.jsonl",
        "format": "claude-jsonl"
      },
      "panes": [
        { "server": "/tmp/tmux-1000/default", "id": "%3", "evidence": "process", "view": "main" }
      ]
    }
  ],
  "panes": [
    {
      "server": "/tmp/tmux-1000/default",
      "id": "%3",
      "target": "work:1.0",
      "cwd": "/home/me/src/api",
      "command": "claude",
      "agent": "claude",
      "session": "1787a44e-244c-4c3c-aeea-40a74ff7ef8c",
      "evidence": "process",
      "view": "main"
    }
  ]
}
```

`runtime` is `terminal` for a session in its terminal's own agent process and
`service` for one in a background service (the Codex daemon, Claude Code's
supervisor). `transcript` is where the agent writes the session's transcript,
in the agent's own format (`claude-jsonl` or `codex-rollout`); tmag only says
where it is, and the file may not exist before the first turn. `subagents`
lists the session's running subagents. `capabilities` lists the verbs tmag
can carry to the session (see [Control](#control)); each still checks its
gate when used. `requests` are the prompts it waits on (see
[Answering](#answering)). A pane without `session` runs an agent tmag has no proof for.

`tmag watch` prints one JSON document per line: first
`{"type":"snapshot","seq":N,"sessions":[…],"panes":[…]}`, then
`{"type":"event","event":{…}}` for every event after `seq`, and
`{"type":"panes","panes":[…]}` whenever the pane list changes (an agent starts
or exits in a pane, a pane moves or closes, a pane switches between a
session's main agent and a subagent). Events carry `seq`, `time`, `agent`,
`session` and `event`. An event with `state` reports the session in full:
`state`, `kind`, `turn`, `cwd`, `runtime`, `transcript`, `subagents`,
`capabilities` and `requests` are
then what the session is, and a field left out is empty. Every change to any
of them is such an event. `event` is the hook's name, or one of:

| Event | Meaning |
| --- | --- |
| `Attach` | A pane (`pane`) started showing the session, by `evidence`. |
| `Detach` | A pane (`pane`) stopped showing the session. |
| `Send` | `tmag send` delivered a prompt `via` `pane` (then through `pane`) or `service`, from the sending pane `from`, `bytes` long. |
| `Halt` | `tmag interrupt` delivered an interrupt `via` `pane` (then through `pane`) or `service`, from `from`. For Claude Code it reports the session, now idle. |
| `Answer` | `tmag answer` delivered a `decision` (`allow` or `deny`) on `request`, `via` `hook` or `service`. It reports the session: through the hook with the request closed, through the service before Codex reports what followed. |
| `Exit` | The session's process exited without `SessionEnd`; the session is gone. |

A session changes only through events, so a client that applies
events to the snapshot and replaces its pane list with each `panes` document
stays current. Changes that come without a hook (a pane closes, an agent is
killed, a title shows another thread) are found by whichever tmag process
looks next, `watch` itself every two seconds, and recorded once as events.
A change is logged even when the tmag process that made it dies before
logging it: the next one to touch the session logs it. An event may therefore
repeat, but none is lost. After a disconnect, `tmag watch --since <last seq>`
replays what was missed
and sends the pane list, or starts over with a new snapshot if those events
were already rotated away.

Over SSH, a remote client needs nothing else: `ssh host tmag watch`.

Sequence numbers increase across the life of the state directory and start from
the current time when it is created, so a cursor kept from before
`tmag uninstall --purge` is recognised as stale. Treat them as opaque, ordered
integers.

Inside tmux, the same state is in pane options, read-only, for formats and
scripts that do not run tmag: `@tmag_agent`, `@tmag_session`, `@tmag_state`,
`@tmag_kind` and `@tmag_since` (Unix seconds) on every attached pane, and
`@tmag_cmd`, the pane's command when they were written. For example
`tmux list-panes -a -F '#{pane_id} #{@tmag_agent} #{@tmag_state}'`.

Exit codes: `0` ok, `1` error, `2` usage, `3` refused by the safety gate, `4`
timeout. With `--json`, errors are also printed as
`{"schema":"tmag/v1","error":{"code":…,"message":…}}`; codes are `usage`,
`not_found`, `ambiguous`, `refused:unbound`, `refused:busy`,
`refused:background`, `refused:subagent`, `refused:changed`,
`refused:idle`, `refused:unsupported`,
`ended`, `timeout` and `error`.

## Footprint

| Piece | Where | Removed by |
| --- | --- | --- |
| tmux plugin and `tmag` | `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/` | removing the `@plugin` line, then `prefix + alt + u` |
| Agent plugins | each agent's plugin directory | the agent's plugin manager |
| Pane state | `@tmag_*` pane options, gone with the pane | `tmag uninstall --purge` |
| Sessions and event log | `$XDG_STATE_HOME/tmag` (default `~/.local/state/tmag`): one small file per live session, at most two 8 MB event files, and `req/<id>.sock` while an answer hook waits | `tmag uninstall --purge` |

`tmag uninstall` prints the exact removal steps.

## Limitations

- Before an agent's first hook (startup, folder-trust dialogs) it shows as
  `unknown`.
- Claude Code does not report a turn interrupted with Esc, and may not send its
  idle notification afterwards either: a session interrupted in its terminal
  shows `working` until its next prompt. One interrupted with `tmag interrupt`
  is idle at once.
- The Codex title items are not a stable interface. tmag reads the
  `thread-id` item of Codex 0.166: the thread id's first 29 characters and
  `...`, in any position. If a Codex release changes it, Codex panes stop
  showing their sessions; `tmag doctor` then warns while Codex sessions run in
  the daemon and no Codex pane shows one.
- A Codex thread exists before its first prompt, but its hooks start with that
  prompt: until then the pane shows `unknown`.
- Codex reports a daemon thread's end about a minute after its last terminal
  closes, when the daemon unloads it.
- A pane running `claude attach` cannot be proven to show its background
  session (see [Sessions and panes](#sessions-and-panes)).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
