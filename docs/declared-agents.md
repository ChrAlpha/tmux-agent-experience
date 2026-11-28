# Reporting to tmag from an agent it does not know

tmag knows Claude Code, Codex, Pi and oh-my-pi, and ships plugins for them.
Any other coding agent can report itself to tmag through the same command
their hooks run, under its own program's name. Its sessions then show in
tmag's list, watch, picker, status line and `jump`; other agents and the
person reach it with `send`, `interrupt`, `read --last`, `read --turns` and
`reply`, with tmag's first line, obligations and gates; and it reaches them
with the same verbs. [ADR 0005](adr/0005-agents-report-themselves.md)
records why it works this way.

An agent that reports nothing is the person's to tmag: what its tools run
passes for the person's commands. The person can count its program as an
agent with `@tmag-agents` (see [Agents that report nothing](#agents-that-report-nothing)).

## The hook

Run, for each event below, in the order the events happen:

```sh
tmag hook PROGRAM < payload.json
```

- **PROGRAM** is your agent's program as the process table names it: the
  name of its process, as its executable gives it or your agent sets it
  (`process.title` in Node), or the file name of the script an interpreter
  runs when the script is its first argument, as a script's `#!` line runs
  it: `node /usr/local/bin/myagent` runs `myagent`. tmag also reads `python
  -m MODULE` (the module's package), `bun run SCRIPT` and `deno run SCRIPT`,
  and guesses at nothing else: an interpreter given options before its
  script proves no program, so an agent started that way names its process.
  PROGRAM is lowercase letters, digits, `.`, `_` and `-`, starting with a
  letter or digit, at most 32 characters, and none of `claude`, `codex`,
  `pi`, `omp` (or a program tmag takes for theirs), a shell or a program
  that runs one (`sudo`, `env`, `script`, …), an interpreter, `tmux` or
  `tmag`.
- **The process that runs it** must be your agent's process or one of its
  descendants, with no other agent tmag knows in between. The nearest such
  process that runs PROGRAM is the session's host: tmag attaches the pane it
  runs in, takes the commands it runs as your agent's, and ends the session
  when it exits. A hook that runs under no process of PROGRAM records
  nothing, and `tmag doctor` counts it as failed.
- **A session stays with its process.** Run every hook of a session under
  the same process. Another process of PROGRAM that reports the session, as
  a resume does, takes it over only once the first has exited; until then
  its hooks record nothing. It keeps no inbox the first served: name yours.
- **It never fails your agent.** It exits 0, writes nothing on stdout and
  finishes within 3 seconds; failures go to the state directory's `hook.log`
  as metadata. Run it in the order of the events, without waiting for it on
  your agent's path when you can.

To find the tmag the tmux plugin installed, as tmag's own plugins do, run
this command through `sh -c`, with PROGRAM in place:

```sh
for r in "${TMUX_PLUGIN_MANAGER_PATH%/}" "$HOME/.tmux/plugins" "${XDG_CONFIG_HOME:-$HOME/.config}/tmux/plugins"; do case $r in /*) t="$r/tmux-agent-experience/bin/tmag"; [ -x "$t" ] && exec "$t" hook PROGRAM 2>/dev/null;; esac; done; t=$(command -v tmag) && exec "$t" hook PROGRAM 2>/dev/null; exit 0
```

## The payload

One JSON object, in the field names Claude Code's and Codex's hooks use.
tmag reads these fields and ignores every other, prompts and tool inputs
included: it stores no content.

| Field | Meaning |
| --- | --- |
| `hook_event_name` | the event, below; required |
| `session_id` | the session: up to 128 letters, digits, `.`, `_` and `-`, not starting with one of the last three; required |
| `cwd` | the directory the session works in |
| `turn_id` (or `prompt_id`) | the turn the event belongs to |
| `tool_name`, `tool_use_id` | the tool call a tool event is about |
| `agent_id` | the subagent the event is about, when it is not the main agent's |
| `trigger` | why a compaction runs: `manual` or `auto` |
| `error` | why a failed turn ended, for `StopFailure` |
| `inbox` | the absolute path of your agent's inbox socket (see [The inbox](#the-inbox)) |
| `mode` | `tui` when a person works in your agent's terminal; any other value when it serves a program, which tmag then neither shows in a pane, nor controls, nor asks for turns |

Paths longer than 4096 bytes are dropped, and other values are cut at 256
bytes.

## The events

Each keeps the meaning Claude Code's and Codex's hooks give it.

| Event | When | tmag records |
| --- | --- | --- |
| `SessionStart` | a session starts or resumes (`source`: `startup`, `resume`, `clear`; `compact` changes nothing) | the session, idle; a resumed one may name its last turn |
| `UserPromptSubmit` | a prompt starts a turn | working, in that turn |
| `PreToolUse` | a tool call starts | the call |
| `PostToolUse`, `PostToolUseFailure` | a tool call ends | the call's end, and of a permission prompt about it |
| `PostToolBatch`, `PreModelRequest` | the calls of a step are settled, or the agent asks its model again | every call and prompt of that agent settled |
| `PermissionRequest` | the agent asks the person to approve a tool call | blocked on a permission |
| `Elicitation`, `ElicitationResult` | the agent asks the person a question that is about no tool call, and has its answer | blocked on a question, then not |
| `SubagentStart`, `SubagentStop` | a subagent (`agent_id`) starts or stops | the subagents running |
| `Stop`, `Interrupt` | a turn ends, or an interrupt ended it | idle |
| `StopFailure` | a turn failed | idle, with `error` as its reason |
| `PreCompact`, `PostCompact` | a compaction starts and ends | compacting, then working again (`auto`) or idle (`manual`) |
| `SessionEnd` | the session ends | its end |

Name every turn with an id of its own, and give the same `turn_id` to every
event of that turn: tmag tells that a prompt it delivered started a turn from
a turn id other than the last, and reads the reply of the turn your hooks
named last.

## The inbox

Serve a Unix socket from your agent's own process, the host the hook proved,
and name its path in every payload's `inbox`. Make it reachable to the user
alone, as tmag's extensions do with `/tmp/tmag-<uid>/inbox/PROGRAM-<pid>.sock`
in directories of mode 0700. tmag proves a connection by its peer: the
process that serves the socket must be the session's host, still running.

Each connection carries one request, a JSON line, and your answer, a JSON
line. Answer `{"ok":true}` for a request you carried out, or `{"ok":false,
"refused":REASON,"error":TEXT}` for one you did not; `{"ok":false,
"error":TEXT}` without a reason says you took nothing for another cause.
Refuse `changed` when the request names a session your agent no longer
serves.

| Request | What your agent does |
| --- | --- |
| `{"send":TEXT,"session":ID}` | gives TEXT to the model as a user message now: it starts a turn when idle, and joins the running one at its next step otherwise |
| `{"interrupt":true,"session":ID}` | stops the running turn, then reports `Stop` or `Interrupt`; refuses `idle` when no turn runs, or `unsupported` |
| `{"turns":N,"session":ID}` | answers `{"ok":true,"turns":[…]}`: its last N turns, oldest first, each `{"id":TURN,"prompts":[{"text":TEXT}],"reply":TEXT}`, the prompt that started it first; the last may still run; refuses `unsupported` when it keeps none |

A prompt another agent sends starts with a line tmag writes, `[tmag] From
AGENT SESSION, another agent.` and what to do with it: give it to your model
as it is. `read --last` reads the reply of the turn your hooks named last
through the turns request, and `read --turns` up to 20 turns; tmag prints
them and keeps nothing. Answer a turns request with no more than 64 MiB.

Your agent's tools run their commands below its process, so a `tmag` command
your model runs is your agent's: tmag writes its first line and names your
session there, as long as the process serves one session. tmag carries no
answer to a permission prompt of yours, types nothing into your agent's
terminal and keeps none of its permission settings: a Claude Code session
that bypasses its prompts holds your agent's messages for the person.

## Checking it

With your agent running in a tmux pane, `tmag list` shows its session in that
pane and `tmag doctor` counts failed hooks. Without your agent, a hook run
from a shell records nothing:

```sh
printf '{"hook_event_name":"SessionStart","session_id":"s1"}' | tmag hook myagent
tmag doctor
```

## Agents that report nothing

`@tmag-agents`, unset by default, names programs whose commands are an
agent's, separated by spaces or commas:

```sh
set -g @tmag-agents 'myagent otheragent'
```

A `tmag` command one of them runs is then an agent's, though tmag knows no
session of it: it may not type with `keys`, answer for the person or manage
credentials, its prompts start with tmag's line, and `send --reply` and
`reply`, which need its session, are refused. tmag shows nothing of what the
agent does.

A start template may run a program that reports itself, as any agent:

```sh
set -g @tmag-start-mine 'myagent --some-flag'
```
