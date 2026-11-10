# ADR 0002: Agents work with each other through tmag

- Status: accepted; amended 2026-11-10 (when agents look at peers, and the
  `next:` line after refusals); amended again 2026-11-10 (one `send` verb,
  and replies through the asker's channel)
- Date: 2026-11-04
- Builds on: [ADR 0001](0001-agent-control-surface.md), whose verbs agents
  use here unchanged.
- Evidence: Claude Code 2.1.317 and 2.1.318 (Haiku) and Codex 0.166, observed
  live in both directions (see [Checked live](#checked-live)).

## Context

ADR 0001 made the control verbs safe for agents to run on one another, and
observed that agents do not use them unprompted: asked to message another
agent, Claude Code typed into its pane with `tmux send-keys`, judged its state
from the screen, and once pressed Enter twice. Four lines describing the
verbs were enough for it to use them instead.

Three more things were left to the model and went wrong in the same way:

- **Who sent a prompt.** A prompt carries no sender. A model asked to sign
  its prompt writes the id it believes it has, or none.
- **How to answer.** The sender had to spell out a reply command naming its
  own session, and the receiver had to follow it.
- **Reading the answer.** `tmag read` prints pane text: cut at the pane's
  height, wrapped, mixed with the agent's interface, which is the screen
  reading the verbs were meant to replace.

Comparable tools settle each of these in the tool rather than the model:
ccmux writes a provenance header on handoff and reads the last reply from the
transcript; ae's `ask` hands back a request id and a reply command ready to
run; Claude Code's agent teams mark another session's message in the harness.
None of them works for both Claude Code and Codex while keeping tmag's
invariants: ae injects its context by launching the agents, and an MCP
mailbox stores messages.

## Decision

### Agents learn the verbs from a skill in a plugin of its own

Each host gets a second plugin, `tmag-peers`, holding one skill, `peers`: when
to look at and act on another agent, the verbs, and never to fall back to
tmux. An agent looks when the person asks it to involve one, or on a signal
that a peer shares its work: changes it did not make appear in its
workspace, or it is about to switch, rebase or reset branches, force-push or
stop processes others may rely on. It never checks peers routinely, which
would spend every session's context on peers that do not matter. It acts on
a peer when the person asks, when a peer's prompt asks for something within
the person's work, or to settle a collision with a peer in the same
repository, leaving that peer's changes alone; a working peer is sent one
short message when that settles it. What to do after a refusal or
an error is not in the skill: tmag prints it on the next line, starting with
`next:`, so an agent reads it only when it meets one. The skill's description
names the cases that should load it: those signals, and a prompt that starts
with `[tmag] From` or `[tmag] Reply from`.

- **A plugin, not a prompt injection.** Hooks may not write to stdout (they
  fail open, and the agents put hook output into the model's context), and
  tmag launches no agent, so a skill's description is the only text tmag can
  put in every session.
- **A plugin of its own.** Neither host turns off one skill of a plugin
  reliably. The skill's description is in every session, so it is a default
  like the others: `@tmag-peers` (default `on`) makes `tmag setup` install,
  update or remove the plugin, and `tmag doctor` reports it.
- **The skill names only what the binary has.** `TestPeersSkillMatchesCLI`
  checks every command, flag, refusal code and exit code it names against the
  CLI, and the Codex copy is the Claude copy byte for byte;
  `TestEveryRefusalSaysWhatNext` checks that every refusal has a `next:`
  line.
- **The binary the hooks run.** The plugin's launcher, `binpath.Launcher()`,
  runs the same binary the agent's hooks run: on `PATH` in Claude Code, which
  puts a plugin's `bin/` there, and beside the skill in Codex, which does not.

### tmag writes who sent a prompt

When an agent sends a prompt or a message, `query.Send` writes its first line
from the caller it proved (ADR 0001, Callers): the agent, its session, and
how to answer. A person's prompt gets none. There are four lines:

```
[tmag] From claude <session>, another agent. Answer in your final message.
[tmag] From claude <session>, another agent. When done, send your answer on stdin to: tmag reply <session> -
[tmag] Reply from codex <session>, another agent, to your prompt. Do not reply to it.
[tmag] From claude <session>, another agent. No answer needed.
```

The first is what `send --wait` writes, for a sender that waits for the turn
and reads the answer itself; the second is what `send --reply` writes, for a
sender that ends its turn and takes the answer as its next prompt; the third
is what `tmag reply` writes, and nobody answers it, so two agents do not
reply to each other forever; the fourth is what `send` writes, for something
a peer should know now. Claude Code carries the sender's name in its own
envelope as well: `<agent> <first 8 characters of its session> via tmag`, or
`tmag` for a person or owner client. tmag says nothing about the sender's
permission mode.

No prompt or message given to `send` or `reply` may start with `[tmag]`, in
any case and after leading blanks (a usage error), so a first line that did
was written by tmag about an agent it proved. Like the rest of the gate this
holds for what tmag delivers; text typed into a pane by other means can say
anything.

A send without a flag delivers now through the session's own channel, or
through its pane while it is idle; `--wait` needs an idle session and starts
a turn of its own; `--reply` needs a caller whose session tmag proves.
`reply` needs one too, because the reply would have nowhere to go otherwise.
It delivers now through the asker's own channel, whatever its state, with
the reply line; a pane-only asker gets it through its pane while idle, and
while busy it retries until `--timeout` on busy refusals alone. It times out
with exit 4; it says nothing was delivered, and may run again, only when its
last try was refused, which delivers nothing. After any other error the
delivery is unknown, and the reply is not sent again.

### Answers are read from the transcript

`tmag read ID --last` prints the reply that ended an idle session's last turn,
read backward from the agent's transcript at the moment of the call and kept
nowhere:

- Claude Code: the text of the last assistant message after the turn's last
  user entry, whose `promptId` is the hooks' `prompt_id`.
- Codex: `last_agent_message` of the `task_complete` event naming the turn.

It reads only the turn the session's hooks reported, waiting up to three
seconds for the agent to write that turn's end, so it never returns an
earlier turn's reply. It needs no pane, so it reads sessions in the Codex
daemon and Claude Code's background sessions too. `read` without `--last`
keeps printing pane text, as tmag 0.1.0 published it.

This is the one transcript reader. It extends "no content is stored" as
`answer-hook` did for tool input: tmag holds the reply only to print it.

## Consequences

- An agent with the skill works with another one in two calls (`send --wait`,
  `read --last`) or one on each side (`send --reply`, `reply`), and every
  prompt it sends says who sent it and how to answer, whatever the receiver
  knows. An agent without the skill can follow the first line alone.
- tmag reads two transcript formats that change with agent releases.
  `internal/transcript` records the versions it was written against, and
  AGENTS.md says how to check a new one.
- Whether an agent loads the skill unprompted remains the model's choice. The
  description and the first line's marker are all tmag can offer, and it uses
  both.
- Settled: a reply is delivered now through the asker's own channel when its
  driver has one; a pane-only asker still waits for its `--timeout` while it
  is busy.

## Alternatives considered

- **The sender writes its own header**, as the skill first asked. A model
  writes the id it believes it has, or none, and any model can write any
  header; tmag already knows the caller.
- **Injecting the protocol at launch**, as ae does. It needs tmag to start
  the agents, which a tmux plugin does not.
- **A mailbox or an MCP server.** It stores messages and runs beside the
  agents, against two invariants, and adds nothing the verbs lack. The
  agents' own inboxes are different: the queue lives in the agent, tmag
  stores no message, and the socket belongs to the session it reaches.
- **Changing `read` to read the transcript.** `read` printing pane text is in
  the published contract; a new flag keeps it.
- **One skill in the `tmag` plugin.** The person could not keep it out of the
  agents' context without losing the hooks.

## Checked live

- Claude Code 2.1.317 (Haiku), asked only to involve another agent, loaded the
  skill and used `list`, `send --wait` and `read --last`, and `send --reply`
  answered by `reply`; the receiver saw tmag's first line with the sender's
  full session id.
- Claude Code 2.1.318 and Codex 0.166 (`--no-daemon`, in a private tmux
  server), each asked only to ask the other: Claude used `send --reply`, Codex
  followed the first line with the skill's `scripts/tmag reply`, and Claude
  reported the answer; Codex used `send --wait` and `read --last` on Claude.
  `read --last` named the turn the hooks reported for both agents.
- Claude Code 2.1.322 (Haiku 5.5), working in a repository where a fake peer
  changed a file it was editing: with the description listing the signal
  among the other cases, it noticed the change and kept it, but loaded the
  skill in none of 6 runs; with the description leading with why such a
  change matters (another agent may be editing the same repository), it
  loaded the skill and ran `tmag list` in 4 of 6. Neither description
  loaded it in a task without a signal.
- Claude Code 2.1.319 (Haiku), 4f0937c: an interactive session's listener was
  the process the hooks recorded; an idle send through the inbox started a
  turn and `read --last` returned its marker; a working session absorbed the
  send, and a second send was refused `busy` with its next step named; a
  bypass session held the send while tmag reported it written, and an
  explicit accept delivered it; a background session's inbox was served by
  its worker, not its pty host, and both idle and working sends reached it;
  a busy asker took an agent's reply through its inbox, an idle one through
  its channel.
- Pi 1.5.0 and oh-my-pi 18.18.6, 9c27e4e: each real hook payload named its
  per-process inbox under `/tmp/tmag-<uid>/inbox` with 0700 directories; an
  idle send started one run and `read --last` returned its marker; a working
  send joined the running run (Pi as `steer`, oh-my-pi as `aside`) with one
  uninterrupted `sleep 20` and one agent lifecycle, and the reply included
  it; a normal exit removed the inbox; a busy Pi asker took an agent's reply
  through its inbox, an idle one through its channel. The full suite, race suite, lint,
  shellcheck, govulncheck and marketplace validation passed there.
- Claude Code 2.1.319 (Haiku), 1793caa: with only the peers skill, in 4 of 4
  samples (three given the full request, one the short one) it loaded the
  skill, ran `tmag list` and then `tmag send` to a Pi that was working, and
  the Pi received the message through its inbox. It never used `--wait`,
  polling or raw tmux. In default permission mode the person confirmed the
  Bash commands Claude chose.
- Not checked: Codex in its shared daemon (the verbs go through its control
  socket, covered by the fake daemon in e2e) and Codex's sandbox, which the
  machine used could not run.
