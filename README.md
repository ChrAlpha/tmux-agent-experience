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
   `/hooks`.

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
| `@tmag-release-url` | GitHub releases | where to download the binary, for mirrors |

Set a key option to `off` to leave it unbound, and `@tmag-alert` to `off` for
no bells. Set options before TPM's `run` line.

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

$ tmag send 01a19fda --wait "Review the diff in HEAD and list risky changes"
01a19fda idle

$ tmag read %3 --lines 40      # recent pane output
$ tmag wait 1787 --for idle    # block until the agent is idle
$ tmag watch                   # JSON lines: a snapshot, then every event
```

A target is an agent id or a unique prefix of at least 4 characters, a pane id
`%N` on the current tmux server, or `SERVER:%N` with a socket name (as for
`tmux -L`) or path.

### States

| State | Meaning |
| --- | --- |
| `working` | A turn is running. Kind `background` means the turn ended but background tasks still run. |
| `blocked` | The agent waits for you. Kind `permission` (tool approval) or `question`. |
| `idle` | Waiting for the next prompt. Kind `error` after a failed turn. |
| `unknown` | An agent process is running but has reported nothing yet (startup, trust dialogs, plugin not installed), including an agent restarted in a pane whose previous agent never reported its exit. |

### Identity

Every agent row carries a `binding`, the evidence that ties its session to its
pane:

| Binding | Evidence |
| --- | --- |
| `process` | The hook descends from the pane's own agent process. Claude Code, and Codex run with `--no-daemon`. |
| `title-id` | Codex: the pane's title shows the thread id (`[tui] terminal_title` with `thread-id`). |
| `session` | The pane's current agent process already carries this session, and for Codex its title does not show another thread. |
| `title-name` | Codex: the pane runs Codex in the session's directory and one item of its title is exactly the thread's name, which no other thread shares. |
| `unbound` | No proof. The agent is listed, but `send` and `wait` refuse it. |

By default Codex runs threads in a shared app-server daemon, which also runs
their hooks, so a hook cannot tell which terminal a thread is shown in. `tmag`
never trusts `$TMUX_PANE` for that reason; it matches the Codex title instead.
A new Codex thread is named after its first turn, so it binds when that turn
ends. Adding `thread-id` to Codex's terminal title binds it immediately:

```toml
[tui]
terminal_title = ["thread-id", "activity", "thread-name", "project-name"]
```

### Sending safely

`tmag send` delivers a prompt only to a bound, idle agent. Right before typing
it re-checks, under a per-pane lock, that the same agent process still runs
the same session and is idle. A busy agent is refused rather than queued, so a
turn's id always belongs to the prompt that started it.

Delivery is a bracketed paste followed by Enter. Control characters are
removed first, so relayed text cannot end the paste early or press keys.

After delivery the agent shows as `working (submitted)` and further sends are
refused until the agent's own hook confirms the turn. Input that starts no turn,
such as a slash command, releases the agent after 15 seconds;
`send --wait` then reports `"started": false`.

`wait` and `send --wait` end with error code `ended` when the session ends or
the agent leaves its pane.

## Machine contract

Every JSON document carries `"schema": "tmag/v1"`.

`tmag list --json`:

```json
{
  "schema": "tmag/v1",
  "seq": 42,
  "agents": [
    {
      "id": "1787a44e-244c-4c3c-aeea-40a74ff7ef8c",
      "agent": "claude",
      "state": "blocked",
      "kind": "permission",
      "turn": "00161c25-78f9-4c24-8799-5ed63c406153",
      "since": "2026-11-03T05:20:28Z",
      "binding": "process",
      "pane": {
        "server": "/tmp/tmux-1000/default",
        "id": "%3",
        "target": "work:1.0",
        "cwd": "/home/me/src/api",
        "title": "…",
        "command": "claude"
      }
    }
  ]
}
```

`tmag watch` prints one JSON document per line: first
`{"type":"snapshot","seq":N,"agents":[…]}`, then
`{"type":"event","event":{…}}` for every event after `seq`. Events carry `seq`,
`time`, `agent`, `session`, `event` (the hook name, or `Send`), `state`,
`kind`, `turn`, `cwd`, `pane`, `binding`, and for `Send` the sending pane
(`from`) and the prompt size (`bytes`). After a disconnect,
`tmag watch --since <last seq>` replays what was missed, or starts over with a
new snapshot if those events were already rotated away.

Over SSH, a remote client needs nothing else: `ssh host tmag watch`.

Sequence numbers increase across the life of the state directory and start from
the current time when it is created, so a cursor kept from before
`tmag uninstall --purge` is recognised as stale. Treat them as opaque, ordered
integers.

Exit codes: `0` ok, `1` error, `2` usage, `3` refused by the safety gate, `4`
timeout. With `--json`, errors are also printed as
`{"schema":"tmag/v1","error":{"code":…,"message":…}}`; codes are `usage`,
`not_found`, `ambiguous`, `refused:unbound`, `refused:busy`, `refused:changed`,
`ended`, `timeout` and `error`.

## Footprint

| Piece | Where | Removed by |
| --- | --- | --- |
| tmux plugin and `tmag` | `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/` | removing the `@plugin` line, then `prefix + alt + u` |
| Agent plugins | each agent's plugin directory | the agent's plugin manager |
| Live state | `@tmag_*` pane options, gone with the pane | `tmag uninstall --purge` |
| Event log | `$XDG_STATE_HOME/tmag` (default `~/.local/state/tmag`), at most two 8 MB files | `tmag uninstall --purge` |

`tmag uninstall` prints the exact removal steps.

## Limitations

- Before an agent's first hook (startup, folder-trust dialogs) it shows as
  `unknown`.
- Claude Code does not report a turn interrupted with Esc; the agent returns to
  `idle` when Claude sends its idle notification about a minute later.
- Codex thread names and title formats are not a stable interface. tmag reads
  the format of Codex 0.166 (items joined by ` | `); `tmag doctor` reports what
  it sees.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
