---
name: flow
description: Load this skill when the user asks to run several coding agents (Claude Code, Codex, Pi, oh-my-pi) in this machine's tmux panes through a process with steps, such as independent reviews whose findings are fixed and re-checked, or work done in units that commands accept, or to write or run a tmag-flow plan, and whenever a tmag-flow run you started ends, halts or prints a next: line. tmag-flow runs a plan written as code over tmag: it starts and asks the agents, runs the checks itself, keeps a journal so the run survives restarts, and halts for decisions.
---

# Running agents through a plan with tmag-flow

A plan is a TypeScript module that says, as code, which agents play which
roles, what each is asked, which commands accept the work, and where a
decision is needed. `tmag-flow` runs it over tmag: it starts each role's
agent in a tmux window, gives it a turn, reads the reply of that turn, runs
the acceptance commands itself, and records every step in the run's journal.
A run that stops for any reason resumes where it was; a step whose outcome
it cannot tell halts for a decision instead of guessing.

Use it when the process has more steps or agents than you should hold in
your own context. For one question to one peer, use tmag directly (the
`peers` skill).

## Running tmag-flow

`tmag-flow` is a command on PATH. Exit 127 means it is not installed: tell
the user (`npm install -g tmag-flow`), and install nothing yourself. It
needs tmag, the tmux plugin, as the `peers` skill does.

Run it as your own tool, never in a tmux window you open: tmag proves who
runs each of its calls from the process that runs it, so a run started by
your tool asks the agents as you, and one started in a window would ask
them as the user. In Claude Code, run it with the Bash tool in the
background; you are told when it exits. Where your tool cannot keep a
process running, give it a time with `--for 8m`: it stops at that time,
exits 4, and continues with `tmag-flow resume RUN`.

```sh
tmag-flow plans                      # the plans that ship
tmag-flow start review --arg dir=. --arg reviewers=codex,claude
tmag-flow status                     # the latest run: roles, steps, halts
tmag-flow resume RUN
tmag-flow decide RUN STEP CHOICE
tmag-flow list                       # every run of this project
```

Add `--json` for a document instead of text. A run's name is printed when it
starts, and runs live in `.tmag-flow/` in the main worktree of the
repository.

With `revision: true`, the runner samples the role's directory immediately
before sending an ask, or the command's directory immediately before running
a gate or exec. The result's `revision` is `<sha>`, `<sha> (dirty)`, or absent.
Review and unit results report a shared revision, `mixed (<value>; <value>)`,
or `revision unavailable`. For commands, `tmag-flow status RUN` shows the
verdict and last non-empty output line. Add `--json` for recorded revisions
and, on command steps, the exit code and last 4096 characters of raw output.

## When it exits

- **0**: the run is done; what the plan returned is printed.
- **5**: a step halted. Read the question with `tmag-flow status RUN`. Decide
  it yourself only when the answer lies within the work the user gave you,
  as `recheck` after you fixed the findings it names; otherwise put the
  question and its choices to the user. Then `tmag-flow decide RUN STEP
  CHOICE`, and `tmag-flow resume RUN`. A decision covers one more attempt
  of its step.
- **4**: the run spent its `--for` time or was stopped: `tmag-flow resume
  RUN`.
- **1** or **2**: an error; the `next:` line says what to do.

The runner's own halts offer `rerun` or `abandon` when it cannot tell what a
step did, as when an agent's session ended during its turn (for an ask, a
resume looks again first), `wait` or `abandon` when an agent's turn outlives
its timeout or other work keeps the agent from it as long, and `rerun` when
the plan changed a step that already ran or started. The runner interrupts
no turn: one that should stop is the user's to interrupt in its pane. A
timeout counts across resumes, so `--for` never extends it.

While a run runs, its roles are its own: do not prompt them yourself. The
user may type into any of their panes; the run reads the reply of its own
turn regardless. An agent that waits on a permission prompt waits for the
user, as with tmag, and so does one that starts at a trust dialog or a login,
as Codex does the first time it runs in a repository: tmag-flow says which
pane, and goes on once the user answers there, or exits 1 naming it if no one
does within the ask's timeout. Tell the user; never answer it yourself.

## Plans that ship

- `review`: reviewers check a directory, each in a session of its own,
  writing a report and a verdict. When any fails, the run halts; fix the
  findings, write what you fixed and why you left any to the file the halt
  names, decide `recheck`, and those who failed check the fixes.
  `--arg dir=PATH --arg reviewers=A,B`, and optionally `--arg brief=PATH`,
  `--arg focus=TEXT` for every reviewer and `--arg focus-NAME=TEXT` for
  reviewer NAME, `--arg rounds=N`, `--arg recheck=all`.
- `units`: an implementer works in a git worktree of its own through units
  in order; after each, the runner runs that unit's acceptance commands, and
  sends failures back. Then reviewers check, the implementer fixes what they
  find, and they check again. `--arg units=PATH --arg implementer=A --arg
  branch=NAME`, and optionally `--arg reviewers=A,B`, `--arg brief=PATH`,
  `--arg attempts=N`, `--arg rounds=N`. The units file is
  `{"units": [{"name": "...", "task": "...", "accept": ["command", ...]}]}`.

An agent is `claude`, `codex`, `pi` or `omp`, or a template the user set in
tmux as `@tmag-start-NAME`; use the user's templates when they name them.

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

- Every step (`ask`, `gate`, `exec`, `decide`, `freeze`) takes a key of its
  own; a resumed run replays the steps whose key and content are unchanged.
- `ask` returns the reply that ended the turn it started. Ask for results as
  files under `flow.path(...)`, and for a short reply that names them.
- `gate` runs a check without effects; `exec` a command with effects, which
  halts if a run was cut short while it ran.
- `decide(key, question, choices)` halts until someone decides.
- `freeze(key, path)` reads a file once, so the run keeps what it held.
- A plan imports only types from `tmag-flow`, and Node's own modules.
- A prompt is a whole task: the agent shares none of your context. tmag-flow
  puts a line naming the run, step and attempt at its top; no prompt may
  start with `[tmag]`.

Start a plan file with `tmag-flow start ./path/plan.ts --arg key=value`.
