<div align="right"><strong>🇬🇧English</strong> | <strong><a href="./README.zh-CN.md">🇨🇳中文</a></strong></div>

# tmux-agent-experience

A tmux plugin that knows what your coding agents are doing.

Keep Claude Code, Codex, Pi and oh-my-pi in your usual panes. See who is
working, waiting or done—and let agents discover each other, send work and
read replies. tmux stays tmux, agents stay agents; tmag is only the thread
between them.

## Your agents, at a glance

- **Know when you are needed.** Blocked and finished agents flag their tmux
  windows with a bell. Use tmux's `prefix + M-n` / `M-p` to visit them.
- **Jump straight to the next agent.** `prefix + A` takes you to the agent
  waiting longest, with blocked agents first. `prefix + a` opens the agent tree.
- **See progress where you work.** Add `#{tmag_status}` to your status line
  for `2 blocked · 1 working`, or `#{E:@tmag-pane-status}` to pane borders
  for labels such as `codex blocked permission`.
- **Let agents work together.** Agents can discover peers, check their state,
  send prompts and read replies. Ask one to bring another into the task.
- **Reach agents beyond the terminal.** Answer permission requests, send prompts
  and stop turns through an optional owner-authorized local HTTP API. Follow
  the same agents from a phone with the [remote companion](client/README.md),
  `tmag-client`. Connect alerts to your own notification command.

No daemon, no wrapper. A prompt is never typed into a busy agent. An agent's
prompt carries its proven sender. Prompts and output are never stored. A
broken or missing tmag never blocks a turn. Defaults are
[configurable](https://github.com/ChrAlpha/tmux-agent-experience/wiki/tmux-Integration#options).

## Install

Requires **tmux 3.2+** on Linux or macOS (amd64 or arm64),
[TPM](https://github.com/tmux-plugins/tpm), `curl`, and any of Claude Code,
Codex, Pi or oh-my-pi.

### Ask your agent

Paste this into your coding agent running inside tmux:

> Read https://raw.githubusercontent.com/ChrAlpha/tmux-agent-experience/main/SKILL.md and follow it to install or update tmag for the AI tool I am using.

The agent asks before editing your tmux configuration, installs the plugin,
connects its own host and verifies the result. Use the same prompt to update.

### Install manually

1. Add this above TPM's `run` line in your tmux configuration, then press
   `prefix + I`:

   ```tmux
   set -g @plugin 'ChrAlpha/tmux-agent-experience'
   ```

2. Press `prefix + M-a` to connect the agents you have through their own
   plugin managers; Pi and oh-my-pi install tmag's packages from the
   plugin's directory. tmag downloads a released binary and verifies its
   checksum.

3. Start fresh agent sessions in tmux. In Codex, approve tmag's hooks with
   `/hooks` and use `/title` to enable `thread-id`. Codex panes attach after
   the first prompt.

**Update:** press `prefix + U`, then `prefix + M-a`. In a fresh Codex session,
reapprove hooks with `/hooks` after each plugin update.

[Full installation, updates and uninstall →](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Installation-and-Updates)

## Use it

| Action | Default key |
| --- | --- |
| Open the agent tree | `prefix + a` |
| Jump to the agent waiting longest | `prefix + A` |
| Set up agent plugins and check installation | `prefix + M-a` |
| Open the local HTTP API in a tmux window | `prefix + M-r` |

For scripts, add the plugin's `bin/` directory to your `PATH`:

```console
$ tmag list
TARGET     PANE  AGENT   STATE                FOR  ID        CWD
work:1.0   %3    claude  blocked (permission) 12s  1787a44e  ~/src/api
work:2.0   %7    codex   idle                 4m   01a19fda  ~/src/web

$ tmag send 01a19fda --wait "Review the diff in HEAD and list risky changes"
01a19fda idle
$ tmag read 01a19fda --last
```

Agents receive the `tmag-peers` skill during setup. Ask yours to have another
agent review a change, investigate a bug or compare approaches.

## Documentation

The [Wiki](https://github.com/ChrAlpha/tmux-agent-experience/wiki) has the guides
and reference:

- [tmux integration and options](https://github.com/ChrAlpha/tmux-agent-experience/wiki/tmux-Integration)
- [Agent collaboration](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Agent-Collaboration)
- [CLI reference](https://github.com/ChrAlpha/tmux-agent-experience/wiki/CLI-Reference)
- [HTTP API](https://github.com/ChrAlpha/tmux-agent-experience/wiki/HTTP-API) and [notifications](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Notifications)
- [Troubleshooting and limitations](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Troubleshooting)
- [Architecture, JSON/events and storage](https://github.com/ChrAlpha/tmux-agent-experience/wiki/Architecture-and-Contracts)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
