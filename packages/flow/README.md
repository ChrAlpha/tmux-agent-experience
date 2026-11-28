# tmag-flow

Plans written as code, run over [tmag](https://github.com/ChrAlpha/tmux-agent-experience):
coding agents in your tmux panes (Claude Code, Codex, Pi, oh-my-pi) through
reviews whose findings get fixed and checked again, units of work that
commands accept, and decisions only you or the agent that started the run
can take.

A plan says, in TypeScript, which agent plays which role, what each is
asked, which commands accept the work and where a decision is needed.
`tmag-flow` runs it: it starts each role's agent in a tmux window beside
yours, gives it a turn, reads the reply of that turn, runs the acceptance
commands itself, and records every step in a journal. A run that stops, for
any reason, resumes where it was; a step whose outcome it cannot tell halts
for a decision instead of guessing.

Why a plan, not a lead agent holding the team in its head: past a handful of
agents, who plays which role, which unit passed and who must check which fix
is state, and state kept in a model's context is what gets lost first. In a
plan it is code and a journal, and the lead agent's context is left for
decisions.

What it builds on: tmag knows which agent runs in which pane and in what
state, delivers prompts only through channels it proves, refuses when that
is unsafe, and writes on each prompt which agent sent it. tmag-flow reaches
agents through nothing else, so every step is a live session you can watch
and type into, and every prompt says who asked.

## Install

You need tmag 0.8.0 or later (the tmux plugin, with its agent plugins set
up), tmux, Node.js 22.18 or later, and Linux or macOS.

```sh
npm install -g tmag-flow
tmag-flow version
```

Agents learn tmag-flow from a skill, `flow`. Claude Code and Codex get it
from a plugin in tmag's marketplace, which `tmag setup` added. Install it for
each agent that should run plans:

```sh
# Claude Code
claude plugin marketplace update tmux-agent-experience
claude plugin install tmag-flow@tmux-agent-experience
# Codex
codex plugin marketplace upgrade tmux-agent-experience
codex plugin add tmag-flow@tmux-agent-experience
# Pi and oh-my-pi: the npm package is their package
pi install "$(npm root -g)/tmag-flow"
omp plugin install "$(npm root -g)/tmag-flow"
```

## Quick start

Ask an agent, or run it yourself in a tmux pane of the repository:

```sh
tmag-flow start review --arg dir=. --arg reviewers=codex,claude
```

Two reviewers start in windows beside yours and review the repository
independently, each writing a report under the run's directory and ending
with a verdict. Immediately before sending each review or recheck prompt,
the runner samples Git in that reviewer's directory. The answer records
`<sha>` for a clean worktree or `<sha> (dirty)` for one with changes. If
the runner cannot read HEAD and the worktree state, the field is absent.
If one reviewer fails, the run halts:

```text
tmag-flow: round-1 halted: claude (fail): /repo/.tmag-flow/review-20261126-140200/artifacts/reports/claude-round-1.md. Fix the findings in /repo (1f0b205d4f9552e570b0b2f87379a8c3aef8941d), write what you fixed and why you left any finding as it is to /repo/.tmag-flow/review-20261126-140200/artifacts/responses/round-1.md, then recheck; or stop.
next: tmag-flow decide review-20261126-140200 round-1 recheck|stop, then tmag-flow resume review-20261126-140200
```

Fix the findings and write that response, then:

```sh
tmag-flow decide review-20261126-140200 round-1 recheck
tmag-flow resume review-20261126-140200
```

The reviewer that failed checks your fixes, and your reasons for any
finding you left, in the session it reviewed in. The review result keeps
each reviewer's latest verdict and revision, including earlier passes. If the
revisions agree, `revision` is the shared value. If they differ, the summary lists
the distinct values as `mixed (<value>; <value>)`. A missing sample counts
as `revision unavailable`, which is also the result when all samples are missing.

`units` takes work in units, each with the commands that accept it:

```json
{
  "units": [
    { "name": "parser", "task": "Implement the parser described in docs/parser.md.", "accept": ["pnpm test parser"] },
    { "name": "printer", "task": "Implement the printer.", "accept": ["pnpm test printer", "pnpm lint"] }
  ]
}
```

```sh
tmag-flow start units --arg units=units.json --arg implementer=codex --arg branch=feat/parser --arg reviewers=claude
```

The implementer works in a git worktree of its own on `feat/parser`. After
each unit tmag-flow runs its acceptance commands itself and sends failures
back; a unit that keeps failing halts for you. Once all pass, the reviewers
check the worktree, the implementer fixes what they find, the acceptance
commands run again, and those who found something check again.

Immediately before each acceptance command, including on a retry, the runner
samples the command's working directory. A passed or skipped unit's `revision`
summarizes all commands in its final attempt using the same rules as a review.
A stopped unit's summary appears beside `stopped` in the result. When any
failed command has a sample, the fix prompt lists each failure as
`$ command  (exit N; <revision>)`, using `revision unavailable` for a missing
sample. When all failed commands lack samples, the lines show `$ command  (exit N)`.

`tmag-flow plans` lists the plans that ship, and each plan's file starts
with the arguments it takes.

## How a run works

```text
tmag-flow start  <plan> [--run NAME] [--arg KEY=VALUE]... [--for D] [--concurrency N] [--agents N] [--ask-timeout D] [--json]
tmag-flow resume <run> [--for D] [--concurrency N] [--agents N] [--ask-timeout D] [--json]
tmag-flow status [<run>] [--json]   # completed gates and commands show their verdict and last non-empty output line
tmag-flow decide <run> <step> <choice> [--note TEXT] [--json]
tmag-flow list   [--json]
tmag-flow plans  [--json]
```

A run lives in `.tmag-flow/<run>/` in the main worktree of the repository it
started in (a directory that ignores itself): its journal, and the files its
agents write under `artifacts/`. Only one process advances a run at a time.

**Roles.** A role starts its agent with `tmag start` the first time it is
asked, in the project, a directory or a worktree of its own, and in a window
named after the role when tmag names windows (the release after 0.8.0).
When a role's session ends between asks, its next ask starts another. An
agent that starts at a trust dialog or a login, as Codex does the first time
it runs in a repository, shows no session until you answer it in its pane:
tmag-flow names the pane and waits as long as the ask may take from the
start, then exits naming it again, to resume once you answered.

**Asks.** An ask gives the agent a turn of its own and returns the reply
that ended it, read from the agent's transcript. The prompt's first line
names the run, the step and the attempt, so tmag-flow finds that turn
whatever else happened in the pane, and a resumed run knows whether the
prompt arrived. It never gives a prompt twice unless the evidence shows the
first never arrived: a send its runner died during halts the ask until tmag
shows what it delivered, and when a session ends during its turn, the ask
halts, and another session is given the prompt only once you decide `rerun`.

**Gates and commands.** `gate` runs a check in the runner, never in the
agent, so an agent cannot pass its own acceptance by editing it. `exec` runs
a command with effects. The journal keeps their combined stdout and stderr.
For a completed gate or command, `status` shows the verdict and, when
present, the last non-empty output line (up to 80 characters). The text
summary treats carriage returns as line breaks, strips ANSI escapes and
removes remaining C0 controls except tabs. In `status --json`, `code` is the
exit code and `output` holds the last 4096 characters of raw stored output,
including terminal controls.
Completed asks and commands also include `revision` when a sample was recorded.

**Halts.** A run halts at a plan's `decide`, and on its own when it cannot
tell what a step did (`rerun` or `abandon`; for an ask, a resume looks again
first), when a turn outlives its timeout or other work keeps the agent from
it as long (`wait` or `abandon`; to stop a turn, interrupt it in its pane
first), and when the plan changed a step that already ran or started
(`rerun`). A decision covers one more attempt of its step as it was when it
halted: a step the plan has changed since halts again.

**Exit codes.** 0 done, 1 error, 2 usage, 4 stopped (its `--for` time spent,
or a signal: resume it), 5 halted (decide, then resume). Every failure
prints a `next:` line.

**Budgets.** `--for` bounds an invocation, which then exits 4 and resumes
later; `--concurrency` how many agents work at once (default 8); `--agents`
how many sessions a run may start (default 16); `--ask-timeout` how long a
turn may take from its prompt, or from a decision to wait, and an agent to
show its session (default 1h), across invocations; `tmag start` itself gets
at least 15s, so that opening a window or a worktree is never cut short.

## Who a run is

tmag proves who runs each command from its process ancestry. tmag-flow's
calls are its own processes, so:

- run by an agent's tool, the run asks as that agent: every prompt starts
  with tmag's `[tmag] From <agent> <session>` line, the asks count as that
  agent's in `tmag list`, and tmag's bounds on how far work is passed on
  apply;
- run by you in a pane, it asks as you.

An agent must never start a run in a tmux window it opens: that would make
its plan look like yours to tmag. The skill teaches agents to run it as
their tool; in Claude Code, in the background, so they are told when it
exits.

## Writing a plan

```ts
import type { Flow } from 'tmag-flow'

export default async function plan(flow: Flow) {
  const impl = flow.role('impl', { agent: 'codex', worktree: 'feat/parser' })
  await flow.ask('parser', impl, 'Implement the parser described in docs/parser.md.')
  const { dir } = await flow.bind(impl)
  const test = await flow.gate('parser-test', 'pnpm test parser', { cwd: dir })
  if (!test.ok) await flow.ask('parser-fix', impl, `The tests failed:\n${test.output}`)
  return test.ok
}
```

| Call | Does |
| --- | --- |
| `flow.role(name, { agent, dir?, worktree? })` | declares a role; its agent starts when first asked |
| `flow.bind(role)` | starts the role's agent if needed; its session and directory |
| `flow.ask(key, role, prompt, { timeout?, revision? })` | the reply that ended the agent's turn, with `Answer.revision` when sampled |
| `flow.gate(key, command, { cwd?, timeout?, revision? })` | a check without effects, run again if cut short, with `CommandResult.revision` when sampled |
| `flow.exec(key, command, { cwd?, timeout?, revision? })` | a command with effects, which halts if cut short, with the same optional `CommandResult.revision` |
| `flow.decide(key, question, choices)` | halts until someone decides; the choice |
| `flow.freeze(key, path)` | a file's content, read once for the whole run |
| `flow.path(...segments)` | a file in the run's artifacts |
| `flow.args`, `flow.run`, `flow.dir`, `flow.project` | the run's arguments, name, directory and project |

Set `revision: true` to request a sample for an ask, gate or exec. A replay
uses the original step's result, including an absent sample in older journals.

Every step has a key of its own: a resumed run replays the steps whose key
and content are unchanged, so a plan may change below the step it halted at.
A plan imports only types from `tmag-flow`, so the runner runs it wherever
it is installed; Node's own modules are there too. Ask for long results as
files under `flow.path(...)`, and for a short reply that names them.

## What it is not

No daemon, no web UI, no mailbox or board between agents, no roles of its
own, no agent driver: tmag-flow is a process that runs while a plan
advances, and reaches agents only through the tmag CLI. tmag-client shows
the sessions it starts, and you decide halts wherever you run `tmag-flow
decide`. Its journal keeps what its steps returned (the agents' replies and
the commands' output) in the run's directory, which git ignores.

## Developing

In `packages/flow` of tmux-agent-experience:

```sh
pnpm install
pnpm check                                          # Biome, TypeScript, unit tests
pnpm e2e                                            # tmag and fake agents in a private tmux server
pnpm build                                          # dist/, as the package ships it
```

The end-to-end tests build tmag and its fake agent from this repository, or
the checkout `TMAG_SRC` names, and run plans in a tmux server of their own;
nothing reaches your own sessions.
`TMAG_FLOW_CLI` runs them against an installed `tmag-flow` instead of the
sources.
