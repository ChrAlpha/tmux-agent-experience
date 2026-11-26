# ADR 0002: Agents work with each other through tmag

- Status: accepted; amended 2026-11-10 (when agents look at peers, and the
  `next:` line after refusals); amended again 2026-11-10 (one `send` verb,
  and replies through the asker's channel); amended 2026-11-16 (a message
  attests the sender's permission class and names the receiver's session);
  amended 2026-11-17 (agents work as a flat team: an ask is owed until it is
  answered, and tmag bounds waits and how far work is passed on; an agent,
  or the person, starts a peer with `tmag start`); amended 2026-11-26
  (relations stay flat and become legible to the caller: a session keeps
  the agent session that started it, a list marks how each session is tied
  to the caller, a start names its window, and a name addresses one window
  exactly)
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
  tmag starts only the agents `tmag start` opens, changing nothing of how
  they run, so a skill's description is the only text tmag can put in every
  session.
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
`tmag` for a person or owner client. It also attests the sender's permission
class: a person or owner client speaks with the receiver's class, an agent
with its own recorded class, and neither when tmag cannot prove one. The
receiver session tmag recorded is on the message line too. A bypass receiver
holds a message that attests no bypass; tmag still reports the delivery, and
says it waits for the person's approval in the receiver's pane.

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

### Asks are owed until they are answered

Agents' relations are flat: any agent may ask any other, and none is
another's parent. A tree of parents and children would tie five relations
to one edge: who started whom, who owes whom an answer, who waits for whom,
who keeps whom running, and who may direct whom. tmag keeps each apart. Who
started a session is its `started_by` (see [Structure is legible to the
caller](#structure-is-legible-to-the-caller)); tmux and the person keep
sessions running, in windows that stay until closed; the person, the proven
sender and its permission class say who may direct whom. What a tree's edge
would give for answers and waits, tmag gives per ask. An agent's own
subagents stay its business: a tree inside one session, flat relations
between sessions.

- **An obligation per ask.** An agent's `send --wait` opens a `final`
  obligation on the receiver, and `send --reply` a `reply` one, in the
  delivery's mark, under the receiver's lock, logged by the `Send` event,
  which then reports the receiver. Each names the asker, its kind, its depth
  and when it opened, and `list --json`, `inspect --json` and watch report
  them as `owes`. A person's prompt opens none. A session owes at most 16:
  an ask beyond them is refused `owing`, so no ask it took is forgotten. A
  delivery proven not taken takes its obligation back, and an `Owes` event
  reports that.
- **Closed by the answer.** A `final` obligation is bound to the turn its
  prompt starts and closed when that turn ends; one whose prompt starts no
  turn within the pending window is owed no more, and binds no later turn.
  `tmag reply` answers only a `reply` obligation: a reply to an agent that
  asked for none, or for the answer in a final message, is refused
  `unasked`, so the line `Reply from …, to your prompt` is true when tmag
  writes it. The reply claims the oldest such obligation under the
  replier's lock before it is delivered, so two replies at once answer it
  once. The claim names the tmag process delivering the reply and lasts as
  long as that process runs, which the process table proves; no event
  reports it. The obligation is settled, with an `Owes` event, once the
  reply was delivered or may have been, a channel having begun to write it,
  and the claim released when the delivery proved nothing was taken.
- **A reply is invited.** A message tmag queued for an agent holds off the
  next one until the agent reaches its next step (ADR 0001); a reply or a
  notice is not held off by it, since the asker invited it and its
  obligations bound how many come. An agent that asks several peers and
  waits for them in one turn receives every answer at its next step.
- **An end settles what it leaves.** The `SessionEnd` or `Exit` event of a
  session lists what it owed. Whatever ended it then starts a detached `tmag
  notify --ended SEQ`, as alerts are started, so a hook keeps its deadline.
  It reads that end from the log, tells each agent the session owed a reply,
  through its own channel, in the one prompt tmag writes on its own account,
  and closes what other sessions owed the session that ended:

  ```
  [tmag] Notice: codex 01a1e902-… ended without answering your prompt. No answer needed.
  ```

  The notice's `Send` event names the end it tells of as `notice`, and has
  no `by`. Only an end tmag logged names askers, so running the command
  tells nobody of an end that did not happen, and tmag tells them within a
  minute of the end; a later run tells nobody, and still closes what was
  owed to the session that ended. The asker's record keeps the
  ends tmag told it of for two minutes, at most 64, checked and written
  under its lock with the delivery: two runs at once tell it once, a notice
  beyond them waits rather than one being forgotten, and a notice its
  channel proved not taken, or that could not be logged, is taken back for a
  later run to tell. A busy asker is tried again each round, so it holds no
  other asker up. An asker that waits for a final answer learns of the end
  from its wait.
- **No wait closes a cycle, as far as tmag sees.** An agent's `send --wait`
  to a session that waits for it, itself or through others, is refused
  `cycle`: a session waits for another that owes it a final answer while its
  own turn runs. `send --reply` never holds a turn, so it closes no cycle.
  The check reads the records without a lock common to them: two agents
  that ask each other at the same instant can still both wait, until their
  `--timeout`.
- **Work is passed on a bounded number of times.** An ask is one deeper
  than the deepest answer its asker owes to a session that still runs: an
  agent the person prompted asks at depth 1. An ask deeper than
  `@tmag-peer-depth` (default 3; `off` bounds it only at 255) is refused
  `depth`. It bounds asks that await an answer; a message that asks for none
  carries no depth.
- **Waiting for several.** `tmag wait` takes several targets, each a session
  it shows, any or `--all` of them, and `--asked` waits until each agent
  that owes the caller has settled every ask it owes it, ended, or needs the
  person.

### Starting a peer is a verb

`tmag start AGENT|TEMPLATE` opens a tmux window beside the pane it runs in,
in a directory (`--dir`, by default that pane's) or a git worktree of its own
(`--worktree BRANCH`, under the repository's `@tmag-worktrees`, by default
`.worktrees`, added on that branch or reused), waits until the pane shows
the agent's session, and delivers the first prompt as `send` does, with its
first line, obligation and gates. The agent replaces the window's shell, as
it does a person's, and stays tmux's: tmag keeps of it the `Start` event,
which names who started it, in which pane and directory, and by which
template, and the session's `started_by` when an agent's proven session
started it.

- **What a start runs is the person's.** A start names one of the agents,
  or a template the person set as `@tmag-start-NAME`, whose command starts
  with one of them. Nobody else gives tmag a command: over HTTP a start
  names an agent or a template, and an absolute directory.
- **Codex shows its session from a first prompt of tmag's.** It shows its
  session only from its first prompt, so tmag gives it one of its own with
  its command, after `--`, asking it to wait; once that turn is over, the
  task follows through `send`, as to any agent. The task never runs before
  the ask it opens, and never reaches the agent's arguments or the process
  table.
- **A start that fails after the agent started says so.** Its error names
  the session that runs, and its `next:` line never asks to start again,
  which would start another.
- **An agent that shows no session is left in its window.** A trust dialog,
  a login or a missing `thread-id` title item holds it; the start exits 4
  and its `next:` line says where to look.
- **The window takes the caller's `PATH`.** tmux gives a window the `PATH`
  of the client that asks for it, so a start finds the agent the caller
  would.
- **A start may name its window.** `--name NAME` names the window tmux
  opens, which tmux otherwise names after the agent; up to 32 letters,
  digits, `-` and `_`, not digits alone. A name a window of the session has
  already is refused before a window opens: a name addresses one window.

### Structure is legible to the caller

One agent may run many. A person's supervisor agent that ran about twenty
agents over six parts of one task held the map of which agent did which part
only in its own context: it carried the map across summaries, made it again
by hand when a reviewer was restarted, and spent its context on it. Studies
and tools that run many agents agree that the coordinator's context is what
gives out first, and every one that scales bounds whom an agent addresses
and keeps structure outside messages. tmag keeps relations flat and makes
what it proves of them legible, scoped to the caller, without tying any
relation to another:

- **Who started a session is kept.** A start by an agent whose session tmag
  proves writes that session to the started session's record as
  `started_by`, under its lock, before the `Start` event, which carries it
  too; every report of the session includes it, so a client follows it from
  the watch. A person's start, an owner client's and that of an agent whose
  session tmag cannot prove name none. It grants nothing: who may direct a
  session, whom it answers and whether it runs are decided as before.
- **A list marks ties.** `tmag list` marks the caller's own row `(you)` and
  every other with the session that started it and how it is tied to the
  caller, with the parts that hold: `(started by you, owes you, you owe
  it)`; a row with none prints as before. `tmag list --mine` keeps only the
  caller's session, those it started, owes or is owed an answer by, and the
  panes that show them, as text and JSON; a caller tmag proves no session of
  gets a usage error. `tmag inspect` names who started a session and each
  answer it owes.
- **A name addresses one window.** A target that is a plain name is the
  exact name of a window in the tmux session of the pane the caller's
  process tree runs in, whatever `$TMUX_PANE` it inherited, or else of a
  session, as tmux prefers a window; never a prefix or pattern of one, which
  tmux alone takes: tmux 3.3a took `w-1` for `w-10`, so a name whose window
  closed reached another agent. A name two windows share is ambiguous.
  tmux's own syntax (`work:1.0`, `{last}`) resolves as before.
- **The skill teaches the discipline.** Each peer gets a named window, a
  worktree when it writes, and a whole part asked with `--reply`; it answers
  with what it found and where, not all of it; peers answer the agent that
  started them, not each other; one message to each says what to do; and a
  part coordinated by a peer keeps an agent's context for its own work.

Checked with fake agents end to end: one agent started 20 peers in named
windows, found them with `list --mine` beside an agent that was not its, and
the peers answered at once while it waited with `wait --asked --all`; in
three runs on two CPUs with the race detector and three on sixteen without,
each ask settled once, every answer reached it, no hook failed or took
longer than `hook.SlowAfter`, and the watch agreed with the list.

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
- What agents owe each other is state, which a client follows from the
  watch and a person sees on a phone: who works for whom, and who waits.
- An agent that runs many peers finds its own part with `list --mine`, after
  its context was summarized too, and the person tells them apart by their
  windows' names; what tmag keeps of that structure is still ids, kinds and
  times.
  `tmag reply` refuses what no ask invited, which an agent that replied
  unasked meets as a refusal with its next step.

## Alternatives considered

- **The sender writes its own header**, as the skill first asked. A model
  writes the id it believes it has, or none, and any model can write any
  header; tmag already knows the caller.
- **Injecting the protocol at launch**, as ae does. `tmag start` opens an
  agent's window, but changes nothing of how the agent runs: its plugins,
  the skill and its first prompt's line carry what it needs to know.
- **A mailbox or an MCP server.** It stores messages and runs beside the
  agents, against two invariants, and adds nothing the verbs lack. The
  agents' own inboxes are different: the queue lives in the agent, tmag
  stores no message, and the socket belongs to the session it reaches.
- **Changing `read` to read the transcript.** `read` printing pane text is in
  the published contract; a new flag keeps it.
- **One skill in the `tmag` plugin.** The person could not keep it out of the
  agents' context without losing the hooks.
- **Parents and children**, as herdr-core keeps them: a child ends with its
  parent, which waits for its children and hears when they stop. It ties
  every relation to the session that started another, which a peer that
  several agents ask, or that the person took over, does not have; and a
  child that ends with its parent ends work the person may want to keep.
- **A shared board of messages or documents**, as Codex's message board and
  herdr-core's documents keep. It stores what agents write, and the
  repository the agents share already holds their work.
- **Teams, roles or a fixed lead**, as Claude Code's agent teams and
  role-based orchestrators keep. Each ties membership or authority to the
  session that formed the team, which a reviewer serving two agents, or a
  peer the person took over, does not have, and a team planned up front is
  not what an agent that involves peers as the work unfolds has.
- **A send to several agents at once.** Tools that run many agents restrict
  broadcast more than any other primitive; one message to each, saying what
  to do, costs a shell loop and is gated and recorded per session.
- **Who works for whom on pane borders.** What a session owes changes on
  sends, replies, ends and expiry, outside its own hooks: a border would
  either take a pane write on each of those paths or keep naming an asker
  that ended. `list`, `inspect` and the watch read it from the records.

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
