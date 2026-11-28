# ADR 0005: Agents tmag does not know report themselves

- Status: accepted
- Date: 2026-11-28
- Builds on: [ADR 0001](0001-agent-control-surface.md), whose verbs, drivers
  and callers it extends, and [ADR 0002](0002-agents-work-with-each-other.md),
  whose asks and starts declared agents take part in unchanged.
- Reference: [Reporting to tmag from an agent it does not know](../declared-agents.md)

## Context

tmag adapts itself to each agent it supports. For Claude Code, Codex, Pi and
oh-my-pi it recognizes the agent's processes, maps its hook events, picks the
channels its terminal and services were seen to take, and reads its
transcript, each observed live and checked again as the agent changes. Many
other coding agents run in tmux panes, most with hooks or a plugin API of
their own. Each would cost tmag that work and its upkeep, and an agent whose
maintainers or users want to work with tmag can do nothing until tmag does.

An agent tmag does not know is also the person when it runs tmag. `identify`
finds no agent above its tool's command, so the command may type with
`keys`, answer for the person under `@tmag-answer on` and manage owner
credentials, and its prompts carry no first line and open no obligation. The
gates of every verb rest on telling an agent from the person.

## Decision

### Three kinds of agent, one model

- **Agents tmag knows**, as before.
- **Declared agents** report themselves through `tmag hook PROGRAM`, under
  their own program's name.
- **Agents that report nothing** are the person's, unless the person counts
  their programs as agents with `@tmag-agents`.

All three meet in the same records, events, verbs and gates: what a session
owes, who asked, how deep an ask goes, which waits close a cycle. They differ
in what tmag can prove of them and in which channels it has seen work.

### A declared agent is what the process table proves

`PROGRAM` is a program name (lowercase letters, digits, `.`, `_` and `-`, at
most 32) that is none of the agents tmag knows, nor one they run as, a shell
or a program that runs one, an interpreter, tmux or tmag. A program's name
is its process's, as its executable gives it or the program sets it (Linux
keeps the first 15 bytes of it, and the command line the rest), or the
script an interpreter runs as its first argument, as a script's `#!` line
runs it (`node /usr/local/bin/gemini` runs `gemini`), with `python -m
MODULE`, `bun run SCRIPT` and `deno run SCRIPT`. tmag tells node by its
command line too, as Node 26 names its process `node-MainThread`. It guesses
at no other argument: which of an interpreter's options take a value is the
interpreter's to say and changes with it, so an interpreter given options
before its script proves no program, and an agent started that way names
its process (`process.title` in Node).

The hook's host is the nearest agent above it, when that agent runs
`PROGRAM`; an agent tmag knows met first proves nothing, since the hook then
runs in that agent's tree, whatever it names. A hook with no such host
records nothing, and `hook.log` counts it failed. The name is the same kind
of evidence the process name of an agent tmag knows is, taken from the
process table, never from the payload.

From then on a declared agent is its process, by pid and start, never its
program's name, which any process may share: another process of the same
program is no agent's client until its own hooks report. A session stays
with the process that reported it while that process runs: another
process's hook for it records nothing, so two processes of one program
cannot take a session, or end it, from each other, and one takes it over,
as a resume does, only once the first is gone, keeping none of its inbox.

- **Its pane.** A hook proves the pane its host runs in while tmag finds no
  other client there, and attaches it by process. The attachment then names
  the client: it holds while that process runs in the pane and the session's
  record names it as its host. An agent tmag knows that a declared agent's
  tool runs takes no pane the declared agent holds.
- **Its tools.** A command below the host is the declared agent's (`by`),
  and its session's when the host runs one, as for any agent: tmag writes
  its first line, opens its obligations and refuses it `self`, `keys` and
  answers for the person.
- **Its inbox.** The inbox its payloads name is proven when its listener is
  the host.

### It reports in the vocabulary hooks share

The payload is Claude Code's and Codex's: their field names, and the events
both have, in the meaning they have there, with `PostToolUseFailure`,
`PostToolBatch`, `StopFailure` and `Elicitation` as Claude Code has them and
`PreModelRequest` and `Interrupt` as Codex does. An agent whose hooks follow
Claude Code's needs nothing but the command; any other maps its events once.
`inbox` and `mode` are those tmag's extensions send. Tool names mean nothing
to tmag here: a declared agent's tools are its own.

### Nothing it says grants anything

A declared agent's claims only take away, as Pi's `mode` already does:

- tmag keeps no permission class for it. A class is what Claude Code's
  envelope attests, and a bypass claim would let its messages past a Claude
  Code session that bypasses its prompts; such a receiver holds them for the
  person instead, and tmag says so.
- tmag keeps no transcript for it: it reads no format but the agents' it
  knows.
- A name that is an agent's tmag knows is no declared agent's, so it cannot
  pass for one.
- A session that serves a program (`mode` other than `tui`) takes no verb,
  is asked for no turns, and leaves the pane that showed it.

### Its inbox is its one channel

`send` takes a declared agent's inbox for every caller in every state, and
`interrupt` its interrupt request, after which the agent reports the turn's
end. tmag carries neither through its pane: how a terminal takes a paste, a
draft, an Escape or an open dialog was observed for each agent tmag knows,
and is not for a declared agent. `answer` is unsupported: answering through
a hook needs the agent's dialog on screen while the hook waits, which tmag
cannot check.

The inbox also answers a turns request, the last N turns the agent holds:
`read --last` and `read --turns` read them there, as they read a transcript,
and keep nothing. A declared agent thus answers `send --wait` and `read
--last` as well as `send --reply` and `reply`.

### The person counts agents that report nothing

`@tmag-agents`, unset by default, names programs whose commands are an
agent's. A tmag command below a process of one of them is that agent's,
whose session tmag cannot tell: it may not type with `keys`, answer for the
person or manage credentials; its prompts carry tmag's first line; and
`send --reply` and `reply`, which need its session, are refused. The option
counts callers only: tmag shows nothing of what such an agent does, and no
pane shows it.

### A template starts a declared agent

A start template's first word may run a declared agent's program; `tmag
start` then waits until its pane shows its session and gives it the prompt,
as for any agent. A bare `tmag start PROGRAM` is refused for a declared
agent: what a start runs is the person's, set as a template.

## Consequences

- An agent integrates itself by running one command from its hooks and,
  for prompts, serving one socket. tmag adds no code for it, and owes it the
  stability of the vocabulary: an event of the shared hooks keeps its
  meaning for declared agents; one more is an addition.
- In `tmag/v2`, a session's and a pane's `agent` may be a declared agent's
  program. Its meaning is unchanged; a client accepts an agent it does not
  know, as it does a kind.
- The state directory keeps a declared agent's sessions beside the others,
  under its program's name. This is no layout change: a tmag that knows no
  declared agents reads none of them, as it reads no session of an agent it
  does not know.
- What a declared agent says of its state is what tmag reports, as it is for
  every agent; tmag checks it against nothing but its process.
- Out of reach for declared agents: answers, the pane's channels, transcripts
  read while the agent is gone, and attachment by title.
- Pi and oh-my-pi stay agents tmag knows: tmag ships their extensions,
  recognizes their processes and saw their terminals take a paste and an
  Escape.
- `@tmag-agents` is not a security boundary, any more than ADR 0001's
  callers are: an agent that runs as the user can leave the process tree.

## Alternatives considered

- **Adapting each agent**, as tmag does for the four it knows. It scales with
  tmag's own work, and leaves an agent that wants to work with tmag waiting
  for it.
- **The hook names its host by pid.** A claim narrowed by ancestry, as a
  session variable is; it needs a payload field hooks in Claude Code's shape
  cannot send, and proves no more than the process table's name does.
- **Declared agents known by program name everywhere.** One hook would make
  every process of that name an agent's client and every command below one
  an agent's; the pid keeps it to the process the hook proved.
- **A protocol of tmag's own** (ACP, A2A, an MCP server). ADR 0001's line:
  it wraps the agent and stores or normalizes what it says.
- **Reading the screen for state**, as screen-scraping orchestrators do. It
  names nothing tmag can prove.
- **Requiring `@tmag-agents` before a hook counts.** A hook may not ask tmux
  before it saves; its process proof is the declaration.
- **Transcripts in a format of tmag's.** Each agent would write a second
  transcript for tmag to read; the agent that holds its turns gives them on
  request instead.

## Checked

End to end with the fake agent installed as `acme`, a declared agent, and
`zeta`, an agent that reports nothing: `acme` shows its session in its pane
by process proof, takes a person's prompt and an interrupt through its
inbox and gives `read --last` and `read --turns` their turns, with the
watch's follower agreeing; Claude Code asks it with `send --reply` and its
`tmag reply` settles the ask, reaching Claude Code under its own name with no
class; a start template runs it; a hook outside it records nothing. `zeta`'s
commands are the person's until `@tmag-agents` names it, then an agent's,
refused `keys`, with tmag's first line on its prompts. A second process of
`acme` reporting a session the first still runs records nothing, and takes
it over once the first is gone; a session serving a program is asked for
nothing.

Checked live with opencode 1.19.6 on Node 26, which names its processes
`node-MainThread`, in a private tmux server, its model a local
OpenAI-compatible server: a plugin written against the reference alone,
from opencode's events and its `promptAsync`, `abort` and `messages` calls,
reported its session, attached its pane by process proof, took a person's
`send --wait`, gave `read --last` and `read --turns` their turns, and
stopped a turn on `interrupt`. Its model's bash tool ran `tmag send --reply`
to a fake Claude Code, whose prompt carried tmag's line naming opencode's
session and whose envelope attested no class; Claude Code's `tmag reply`
started an opencode turn and settled the ask, and its `send --wait` and
`read --last` read opencode's answer. An opencode run without the plugin
was the person until `@tmag-agents` named it, then an agent: refused `keys`,
its prompt given tmag's first line. Not checked: macOS, and a published
integration of another agent.
