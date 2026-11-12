# Herdr, and the agents' own remote paths, against tmag

- Date: 2026-11-06. Facts change with releases; each is tied to its source.
- Scope: what a person who runs Claude Code and Codex wants away from the
  terminal (see which agent needs them, see what it asks, decide, prompt,
  stop), and how Herdr and the agents' vendors provide it.

## Herdr

Herdr is three products that do not share running agents
([herdr.dev](https://herdr.dev/)):

| Product | What it is | Version |
| --- | --- | --- |
| Herdr TUI (`herdr`) | A terminal multiplexer that replaces tmux: "It's a server running in the background, and the terminals live inside it" ([/tui](https://herdr.dev/tui/)) | 0.9.5, 2026-10-14 ([releases](https://github.com/herdrdev/herdr/releases)) |
| herdr-core (`hc`) | A resident runtime started through systemd-run or launchd; "Holder processes own the terminals" ([security.md](https://github.com/herdrdev/herdr-core/blob/master/docs/security.md)) | 0.1.18, 2026-11-04 ([CHANGELOG](https://github.com/herdrdev/herdr-core/blob/master/CHANGELOG.md)) |
| Herdr App | A desktop app over herdr-core; update checks hourly and telemetry are on by default ([#207](https://github.com/herdrdev/herdr-app/issues/207)) | 0.1.13, 2026-11-04 ([CHANGELOG](https://github.com/herdrdev/herdr-app/blob/master/CHANGELOG.md)) |

### How it knows what an agent does

- The TUI matches each agent's screen against detection manifests; its Claude
  Code and Codex integrations report identity only ([agents](https://herdr.dev/tui/docs/agents/),
  [integrations](https://herdr.dev/tui/docs/integrations/)). Open:
  AskUserQuestion not shown as blocked ([#5741](https://github.com/herdrdev/herdr/issues/5741)).
- herdr-core reads only OSC 7501 status reports from programs
  ([README](https://github.com/herdrdev/herdr-core)). Hook processes have no
  terminal and cannot report ([core#42](https://github.com/herdrdev/herdr-core/issues/42));
  Claude Code background sessions lose their status
  ([app#343](https://github.com/herdrdev/herdr-app/issues/343)). Agents emit
  OSC 7501 only when the terminal answers its probe, which tmux does not: its
  maintainer closed the request ([tmux#5713](https://github.com/tmux/tmux/issues/5713)).
- The Codex shared daemon breaks the association of sessions
  ([#4649](https://github.com/herdrdev/herdr/issues/4649)); herdr-core's manual
  has Codex run with `--no-daemon`.

### What it can do to an agent

- TUI: `agent start/prompt/wait/read/send-keys`; `prompt` "can prompt an agent
  that is already working" ([automation](https://herdr.dev/tui/docs/agent-automation/)).
- herdr-core: `send` types into an agent after about 3 s without typing, a
  working one too; `interrupt` presses Esc or Ctrl-C
  ([OpenRPC](https://github.com/herdrdev/herdr-core/blob/master/schema/hc.openrpc.json)).
- Neither approves or denies a request, nor shows a tool's input: a client
  answers a permission prompt by pressing keys, which another answer can
  overtake ([core#122](https://github.com/herdrdev/herdr-core/issues/122)).
- A message between agents carries a sender the sending client names itself
  ([SKILL.md](https://github.com/herdrdev/herdr-core/blob/master/crates/herdr-core/src/SKILL.md)).
- herdr-core's event sequence restarts with each core process: "take a fresh
  snapshot after reconnecting" ([machines.md](https://github.com/herdrdev/herdr-core/blob/master/docs/machines.md)).

### Remote and mobile

- Machines are added over OpenSSH; herdr-core "does not add another
  encryption layer or authenticate a remote user"
  ([security.md](https://github.com/herdrdev/herdr-core/blob/master/docs/security.md)).
  An added machine can type into local terminals and fake agent status
  ([core#121](https://github.com/herdrdev/herdr-core/issues/121)).
- There is no official phone client; the request is the most reacted issue of
  the App ([app#306](https://github.com/herdrdev/herdr-app/issues/306)).
  Herdr sends no push notifications. Community clients run their own relays
  or Web Push: Ovis, Collie (a PWA with experimental tmux support), Heeler,
  HerdrUp.

## The agents' own remote paths

- Claude Code Remote Control, generally available since August 2026, reaches
  any interactive `claude`, also in tmux: transcript, prompts, permission
  prompts and questions, push through the Claude app. It needs a claude.ai
  subscription login (not API keys, Bedrock or gateways), keeps the
  transcript on Anthropic's servers, and covers Claude Code only
  ([remote control](https://code.claude.com/docs/en/remote-control)).
- Codex's remote runs through the ChatGPT desktop app as host and OpenAI's
  relay ([remote connections](https://learn.chatgpt.com/docs/remote-connections));
  `codex remote-control` on the CLI is experimental, and its Linux and
  headless story is unsettled ([#35928](https://github.com/openai/codex/issues/35928)).

## Where tmag stands, on `feat/remote`

| Need | tmag |
| --- | --- |
| Keep tmux | tmag is a tmux plugin; panes, keys and sessions stay as they are |
| Know the state | From each agent's own hooks, for terminal sessions, Codex daemon threads and Claude Code background sessions; panes attach only on proof |
| See what a request asks | `tmag request`: the tool and its input, relayed live |
| Decide that request | `tmag answer`: bound to the request id, checked under the session's lock; through Claude Code's waiting hook or the Codex daemon's protocol |
| Prompt, stop | `send` to an idle session only, once; `interrupt` a running turn; through the pane or the Codex daemon |
| Agents with agents | the same verbs, the sender proven from the process table and written by tmag |
| Resume a stream | sequence numbers live with the state directory, `watch --since` resumes |
| From the phone | `tmag serve`: a page in a tmux window, reached through Tailscale or Cloudflare Access, for the devices the person paired from its window, through a tunnel that proves the person too; its only actions are the verbs |
| Told when away | `@tmag-notify`: the person's command per alert, for every session, never with content |
| Agents covered | Claude Code and Codex |

Checked live on 2026-11-06 against Codex 0.166 in its shared daemon, through
`tmag serve`'s page: a thread attached by `title-id`, a prompt and an
interrupt through the daemon, a command approval shown with its command and
allowed by a tap, another denied.
