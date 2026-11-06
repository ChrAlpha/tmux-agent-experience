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
- **Agents working together.** Ask an agent to involve another one, and it
  prompts the other through tmag, waits for its turn and reads its answer:
  delivered once, only while that agent is idle, and recorded.
- **Away from the terminal.** `prefix + M-r` serves a page for your phone:
  every agent on this machine and others you name, exactly what each one asks,
  and the verbs to answer it, prompt it or stop it. A command of yours tells
  you when one waits (see [Away from the terminal](#away-from-the-terminal)).

Underneath, the `tmag` command gives agents, scripts and remote clients the
same state as a versioned JSON contract, and can hand an idle agent its next
prompt safely.

- tmux stays the only terminal runtime: no daemon, no wrapper, no new entry
  point. The page for your phone runs in a tmux window you open.
- Agents report through their own plugin hooks, which fail open: a broken or
  missing `tmag` never blocks a turn.
- Everything lives in the plugin's TPM directory; nothing edits your
  configuration. Prompts and agent output are never stored; `tmag read
  --last` reads an agent's reply from its own transcript when asked.

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
   checks the result. Each agent gets two plugins: `tmag`, whose hooks report
   its state, and `tmag-peers`, a skill that teaches it to work with the other
   agents through tmag (see [Agents acting on agents](#agents-acting-on-agents)).

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
load. Then `prefix + M-a` brings the agent plugins to the same version. Codex
holds an approval of hooks to the plugin version it was given for, so after
each update it asks again: approve tmag's hooks with `/hooks` in a new Codex
session.

### Options

| Option | Default | |
| --- | --- | --- |
| `@tmag-key-picker` | `a` | prefix key for the agent tree |
| `@tmag-key-jump` | `A` | prefix key to jump to the agent waiting longest |
| `@tmag-key-setup` | `M-a` | prefix key to connect the agents and check the installation |
| `@tmag-key-serve` | `M-r` | prefix key to open `tmag serve`, the page for your phone, in a window, or tell where it runs |
| `@tmag-serve` | unset | the arguments `prefix + M-r` gives `tmag serve` (see [The page](#the-page-tmag-serve)) |
| `@tmag-alert` | `blocked,finished` | which changes alert you: ring a pane's bell, and run `@tmag-notify` |
| `@tmag-notify` | unset | a command that tells you of an alert away from the terminal (see [Notifications](#notifications)) |
| `@tmag-answer` | `on` | who may decide an agent's permission prompts and questions with `tmag answer`, beside the agent's own dialog: `on` for people, `agents` for agents too, `off` for nobody |
| `@tmag-peers` | `on` | whether `prefix + M-a` gives the agents the `tmag-peers` skill, for working with each other; `off` removes it |
| `@tmag-release-url` | GitHub releases | where to download the binary, for mirrors |

Set a key option to `off` to leave it unbound, `@tmag-alert` to `off` for
no bells, `@tmag-answer` to `off` to answer prompts only in the agent, and
`@tmag-peers` to `off`, then press `prefix + M-a`, to keep tmag out of the
agents' context. Set options before TPM's `run` line.

## The `tmag` command

Agent hooks find the plugin's binary on their own, and so do agents with the
`tmag-peers` skill. To use `tmag` from a shell or a script, put the plugin's
`bin` directory on your `PATH`, or install the released binary separately with
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
$ tmag read 01a19fda --last    # the reply that ended its turn

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

#### Agents acting on agents

Agents use the same verbs, run from their tools, and tmag tells who runs one
the way it tells which pane shows a session: from the process table. A
command whose ancestors include an agent's process is that agent's; its
session is one of those the agent's process runs, named by the variable the
agent sets for its tools (`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`), or
the only one. The variable alone proves nothing, and when nothing tells which
session it is, the agent is known and its session is not. `tmag list` marks
the caller's own session, and `--json` reports the caller as `caller`.

Every `Send`, `Halt` and `Answer` records where it came from: the pane, as
`from`, and the agent, as `by`. An agent's verb on its own session is refused
with `self`: the turn it would prompt, stop or answer is the one running the
command. An answer stands in for the person's decision, so an agent may give
one only when `@tmag-answer` is `agents`; otherwise it is refused with
`agent`. This keeps an agent from approving its own or another agent's
prompts by accident, and the log shows every decision an agent made. It is
no security boundary: an agent can type into any pane with tmux itself.

Agents do not use these verbs unless told about them: asked to message
another agent, they type into its pane with `tmux send-keys` and judge its
state from its screen. The `tmag-peers` plugin is what tells them. Its skill,
`peers`, covers when to act on another agent (when the person asks, or a
peer's prompt asks within the person's work), the verbs and their refusals,
and never to fall back to tmux. In Claude Code the plugin puts `tmag` on the
agent's `PATH`; in Codex the skill runs it from its own directory. Either way
it is a launcher that runs the binary the agent's hooks run.

A prompt carries no sender of its own, so when an agent sends one, tmag
writes its first line from the caller it proved: which agent and session sent
it, and how to answer. An agent without the skill can follow it too:

```
[tmag] From claude 1787a44e-…, another agent. Answer in your final message.
[tmag] From claude 1787a44e-…, another agent. When done, send your answer on stdin to: tmag reply 1787a44e-… -
[tmag] Reply from codex 01a19fda-…, another agent, to your prompt. Do not reply to it.
```

The first is what `send` writes, for a sender that waits for the turn
(`send --wait`) and reads the answer with `read --last`; the second is what
`send --reply` writes, for a sender that ends its turn and takes the reply as
its next prompt; the third is what `tmag reply` writes. A prompt given to
`send` or `reply` may not itself start with `[tmag]` (a usage error), so a
first line that does was written by tmag about an agent it proved. A
person's prompt gets no such line. An agent with the skill treats such a
prompt as a colleague's request rather than the person's, and does not
answer a reply. Like the rest of the gate, this holds for prompts tmag
delivers; it is no security boundary against text typed into a pane by
other means.

`tmag reply` sends like `send`, but waits for an asker that is busy, until
its `--timeout` (one minute by default), and prompts it once it is idle;
exit 4 then means nothing was delivered. Only an agent whose session tmag
proves can send a reply or ask for one.

#### Reading a reply

`tmag read ID --last` prints the reply that ended a session's last turn,
read from the agent's transcript at the moment of the call: the text of
Claude Code's last assistant message, or Codex's `last_agent_message` of the
turn's end. tmag keeps nothing of it. It reads only an idle session (`busy`
otherwise), and only the turn the session's hooks reported, waiting a few
seconds for the agent to write that turn's end; a turn that ended without a
reply, or a transcript that does not catch up, is the error `no_reply`. It
needs no pane, so it reads sessions in the Codex daemon and Claude Code's
background sessions too. `--json` adds the session and the `turn`. Without
`--last`, `read` prints the pane's recent text.

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
typing reported an error, since the prompt may have arrived all the same.
`send --wait` waits until that turn ends or blocks on a request, and reports
which; input that starts no turn, such as a slash command, releases the
session after 15 seconds, and `send --wait` then reports `"started": false`.

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
`off` it does not wait; `answer` is then refused with `unsupported`, for
every agent.

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

## Away from the terminal

### The page: `tmag serve`

`prefix + M-r` opens `tmag serve` in a tmux window: a page for your phone with
every agent session on this machine, and on others you name. First comes what
needs you: each request with exactly what it asks (the command, the change to
a file, the question and its options) and its answers. Then every session by
state, and for each its last reply, its pane's recent text, a box for the next
prompt while it is idle, and Interrupt while it works. Each action is one tmag
verb, with the gate it always has; the page types no keys and opens no shell.
It runs while that window lives: close the window to stop it. Pressed again,
the key tells where it runs.

It listens on loopback only. To reach it from your phone, put a tunnel in front
that proves who you are, and tell `tmag serve` about it in `@tmag-serve`.

With [Tailscale](https://tailscale.com/kb/1312/serve), on the machine:

```sh
tailscale serve --bg 7777
```

```tmux
set -g @tmag-serve '--tailscale box.tail1234.ts.net --owner you@example.com'
```

The owner is your tailnet login. Tailscale Serve sets it on every request from
your devices and removes it from anything else; a tagged device or a Funnel
request carries none and is refused.

With Cloudflare Zero Trust: a tunnel to `http://127.0.0.1:7777` for a host name
behind an Access application, and

```tmux
set -g @tmag-serve '--access tmag.example.com --access-team yourteam --access-aud AUD_TAG --owner you@example.com'
```

`tmag serve` verifies each request's Access token against your team's keys and
the application's audience tag, and admits your email only.

On the machine itself, open the address the window shows: it carries a token,
kept in tmag's state directory, which keeps other users of the machine out.
On an iPhone, add the page to the Home Screen to use it as an app.

Agents on other machines show on the same page: name each with the command
that runs tmag there, without `-t` for ssh.

```tmux
set -g @tmag-serve '--tailscale box.tail1234.ts.net --owner you@example.com --machine "laptop=ssh laptop tmag" --machine "dev=docker exec -i dev tmag"'
```

`tmag serve` runs `tmag watch` there for as long as the page is open, and one
command for each action; nothing else runs on those machines.

| Flag | |
| --- | --- |
| `--owner WHO` | who may come through a tunnel: a Tailscale login, or the email Access vouches for |
| `--tailscale HOSTS` | host names Tailscale Serve forwards, comma-separated |
| `--access HOSTS`, `--access-team TEAM`, `--access-aud TAG` | host names a Cloudflare tunnel forwards behind Access, your team's name, and the application's audience tag |
| `--machine NAME=COMMAND` | another machine, and the command that runs tmag there; repeatable |
| `--name NAME` | this machine's name on the page; its host name by default |
| `--addr ADDR` | the loopback address to listen on; `127.0.0.1:7777` by default |

### Notifications

The changes that ring a pane's bell (`@tmag-alert`: a session starts waiting
for you, or finishes a turn) can also run a command of yours, for every
session, including those no pane shows, such as a Codex daemon thread: set
`@tmag-notify` to a command that sends a notification. It runs unless you
typed into a tmux client in the last minute, when you are at the terminal and
the bell is there. It runs with `sh`, apart from the agent's hook, which does
not wait for it, and is ended after 20 seconds. `tmag notify` sends a test
alert and shows what the command printed; `tmag doctor` reports its latest
failure.

The command gets the alert in environment variables, never inside its own
text, so quote them as below:

| Variable | |
| --- | --- |
| `TMAG_ALERT` | `blocked` or `finished`; `test` from `tmag notify` |
| `TMAG_TITLE`, `TMAG_MESSAGE` | a title such as `claude needs your approval`, and what and where: `Bash · api · work:1.0 · box` |
| `TMAG_AGENT`, `TMAG_SESSION` | the agent, and the session's id |
| `TMAG_STATE`, `TMAG_KIND` | the session's state and kind, as `tmag list` shows them |
| `TMAG_REQUEST`, `TMAG_TOOL` | the request the session waits on, and its tool |
| `TMAG_CWD`, `TMAG_TARGET` | the session's directory, and the pane that shows it, if one does |
| `TMAG_HOST` | the machine's host name |
| `TMAG_URL` | the request's page, or the session's, in a `tmag serve` running behind a tunnel |

They carry no prompt, reply or tool input: a notification goes to a service
of your choice, and nobody should approve a command they cannot see. Follow
`TMAG_URL` to see it.

With [ntfy](https://ntfy.sh) and its app (the topic is the password, so make
it unguessable):

```tmux
set -g @tmag-notify 'curl -fsS --max-time 10 -H "Title: $TMAG_TITLE" ${TMAG_URL:+-H "Click: $TMAG_URL"} -d "$TMAG_MESSAGE" https://ntfy.sh/your-unguessable-topic'
```

With [Bark](https://github.com/Finb/Bark) on iOS:

```tmux
set -g @tmag-notify 'curl -fsS --max-time 10 --data-urlencode "title=$TMAG_TITLE" --data-urlencode "body=$TMAG_MESSAGE" --data-urlencode "url=$TMAG_URL" --data-urlencode group=tmag https://api.day.app/YOUR_KEY'
```

With [Pushover](https://pushover.net/api):

```tmux
set -g @tmag-notify 'curl -fsS --max-time 10 --form-string token=APP_TOKEN --form-string user=USER_KEY --form-string "title=$TMAG_TITLE" --form-string "message=$TMAG_MESSAGE" --form-string "url=$TMAG_URL" https://api.pushover.net/1/messages.json'
```

With a [Telegram](https://core.telegram.org/bots/api#sendmessage) bot of
yours:

```tmux
set -g @tmag-notify 'curl -fsS --max-time 10 --data-urlencode chat_id=CHAT_ID --data-urlencode "text=$TMAG_TITLE: $TMAG_MESSAGE $TMAG_URL" https://api.telegram.org/botBOT_TOKEN/sendMessage'
```

To be told only when a session waits for you, start the command with
`[ "$TMAG_ALERT" != finished ] &&`.

## Machine contract

Every JSON document carries `"schema": "tmag/v1"`.

`tmag list --json` gives every session and every pane running an agent:

```json
{
  "schema": "tmag/v1",
  "seq": 42,
  "caller": { "pane": { "server": "/tmp/tmux-1000/default", "id": "%5" } },
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
`caller` is who ran the command: its pane, and `by` the agent whose tool ran
it, as in events.

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
| `Send` | `tmag send` delivered a prompt `via` `pane` (then through `pane`) or `service`, `bytes` long. |
| `Halt` | `tmag interrupt` delivered an interrupt `via` `pane` (then through `pane`) or `service`. For Claude Code it reports the session, now idle. |
| `Answer` | `tmag answer` delivered a `decision` (`allow` or `deny`) on `request`, `via` `hook` or `service`. It reports the session: through the hook with the request closed, through the service before Codex reports what followed. |

`Send`, `Halt` and `Answer` carry `from`, the pane the command ran in, and
`by`, the agent whose tool ran it (`agent`, and `session` and `subagent` when
known), when there is one (see [Agents acting on agents](#agents-acting-on-agents)).
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

Over SSH, a remote client needs nothing else: `ssh host tmag watch`. A watch
ends as soon as nobody reads it, even with nothing to send: when its reader
closes the pipe, or when the connection of a command ssh runs without a
terminal drops.

Sequence numbers increase across the life of the state directory and start from
the current time when it is created, so a cursor kept from before
`tmag uninstall --purge`, or from before a reboot, which starts a new realm's
directory, is recognised as stale. Treat them as opaque, ordered
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
`refused:idle`, `refused:unsupported`, `refused:self`, `refused:agent`,
`ended`, `no_reply`, `timeout` and `error`.

## Footprint

| Piece | Where | Removed by |
| --- | --- | --- |
| tmux plugin and `tmag` | `$TMUX_PLUGIN_MANAGER_PATH/tmux-agent-experience/` | removing the `@plugin` line, then `prefix + alt + u` |
| Agent plugins `tmag` and `tmag-peers` | each agent's plugin directory | the agent's plugin manager |
| Pane state | `@tmag_*` pane options, gone with the pane | `tmag uninstall --purge` |
| `tmag serve` | a tmux window, while you keep it open, and `@tmag_serve_url` while it runs behind a tunnel | closing the window |
| Sessions and event log | `$XDG_STATE_HOME/tmag/<realm>/` (default `~/.local/state/tmag/<realm>/`): one small file per live session, at most two 8 MB event files, `req/<id>.sock` while an answer hook waits, and `serve.token` once `tmag serve` ran. The realm names the process table tmag sees, one per container or machine and boot, so containers that share a home directory keep their sessions apart | `tmag uninstall --purge`; `tmag prune`, run by the picker key, removes other realms' state, and the state tmag 0.2.0 and earlier kept in `tmag/` itself, once unused for 30 days |

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
