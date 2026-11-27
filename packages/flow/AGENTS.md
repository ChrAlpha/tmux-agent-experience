# AGENTS.md

Guidance for coding agents working in this repository. README.md introduces
the product; this file says how to change it without breaking its promises.

## What this is

A runner for plans written as code over tmag. A plan is a TypeScript module
whose default export takes a `Flow`; `tmag-flow` runs it, step by step, in
a journal, reaching coding agents only through the tmag CLI. It sits above
tmag the way tmag-client does: a product of its own that consumes tmag's
versioned contract (`tmag/v2`) and changes nothing of tmag.

## Promises

Every change keeps these. Tests guard most of them; treat a failing one as
a design problem.

- **tmag is the only way to an agent.** The runner runs `tmag` with `--json`
  (`src/tmag.ts`): `start`, `send --wait`, `read`, `wait`, `inspect`,
  `list`. Never tmag's HTTP API, whose every request is the owner; never
  `tmux send-keys`, `paste-buffer` or `capture-pane`; never an agent's own
  CLI. A change that needs more of tmag is a change to tmag's
  contract, made there first.
- **A run is who runs it.** Every tmag call is a process of the runner's
  own, so tmag proves it as the agent whose tool runs tmag-flow, or the
  person. No flag, variable or field names a caller. Each invocation
  journals the caller tmag reports.
- **Every effect is a keyed step in the journal.** `src/journal.ts` appends
  and syncs, never rewrites; a line a crash cut short is cut off before the
  next append. A step that is done replays from the journal; one that was
  started picks up where the evidence says it is; one the plan now
  describes differently from the one that ran or started halts.
- **A prompt is given once.** An ask records its intent before it sends and
  its delivery once tmag reports one; the prompt's first line (`tmag-flow
  run R, step K, attempt N`, unique to the intent) finds its turn, and only
  at the top of a prompt. Each send is recorded before its process starts
  and when it ends. A prompt is sent again only when the evidence (tmag
  shows the turns, the tag in none, and no Send after the intent by an
  invocation that sent it) shows it never arrived; a send no record ended
  halts the ask until tmag shows what it delivered, and a send that failed
  is never repeated blindly. A session that ends after it may have been
  given the prompt halts the ask; no other session is given it until someone
  decides `rerun`, and a start no record finished is never taken for any
  session.
- **One process advances a run.** `src/lock.ts` takes the run's lock by
  creating the next of its numbered locks whole, once the highest is
  released or its process ended; no lock is moved or repaired.
- **Halt rather than guess.** What the runner cannot tell halts for a
  decision (`src/errors.ts` names the kinds and their choices). A decision
  answers the step as it was when it halted, covers one more attempt, and is
  used by its effect's record: the step's done or reset, or a use recorded
  right before an effect that is neither. The runner interrupts no turn:
  tmag interrupts a session, not a turn, so it could stop someone else's.
  Every loop that waits is bounded and backs off, and a timeout counts from
  a record in the journal, so no number of invocations extends it.
- **Gates run in the runner.** An agent never runs its own acceptance.
- **Plans need nothing of tmag-flow at run time.** Plans import types only;
  everything is on the `Flow`. No runtime dependencies at all.
- **The skill teaches the CLI as it is.** `skills/flow/SKILL.md` names only
  commands, flags, exit codes, plan arguments and flow calls that exist
  (`test/unit/skill.test.ts`); its copies in the plugins are the same
  bytes, and every manifest has the package's version.
- **Every failure says what next.** An error or halt prints a `next:` line.

## Layout

| Path | Role |
| --- | --- |
| `src/cli.ts`, `src/main.ts` | Entry point; commands, flags, exit codes, output |
| `src/runtime.ts` | The `Flow` behind a plan: steps, replay, roles, asks, gates, decisions, halts, budgets |
| `src/journal.ts` | The run's journal and what it says of each step and role |
| `src/tmag.ts` | Finding and running tmag, reading its documents |
| `src/lock.ts` | One process advances a run |
| `src/project.ts` | Where runs live, run names, finding plans |
| `src/view.ts` | What `status` and `list` show |
| `src/types.ts`, `src/index.ts` | The types plans import |
| `plans/` | The plans that ship (`review`, `units`) and what they share |
| `skills/flow/` | The skill, as the npm package (and Pi, oh-my-pi) ships it |
| `../../plugins/claude/tmag-flow`, `../../plugins/codex/tmag-flow` | The skill as Claude Code and Codex plugins, in tmag's marketplaces, where `tmag setup`'s checkouts find them |
| `test/unit/` | Tests that need no tmux |
| `test/e2e/` | Plans run over tmag and fake agents in a private tmux server |

## Commands

```sh
pnpm install
pnpm check                                          # Biome, TypeScript, unit tests
pnpm e2e                                            # needs Go, tmux and git
pnpm build                                          # dist/, what the package ships
(cd ../.. && claude plugin validate .)              # tmag's marketplace, the plugins included
```

## Conventions

- Node.js 22.18 or later runs the sources as they are: TypeScript with
  erasable syntax only (no enums, namespaces or parameter properties), and
  relative imports with their `.ts` extension, which the build rewrites.
- Biome formats and lints; `pnpm format` applies it.
- Commits follow Conventional Commits. Code comments and docs are in English.
- The version lives in `package.json` and the two plugin manifests; a
  `flow/vX.Y.Z` tag that matches it publishes the package
  (`flow-release.yml`), never as the repository's latest release.

## Testing

- `test/e2e/world.ts` builds tmag and its fake agent from this repository, or
  the checkout `TMAG_SRC` names, runs a tmux server of its own under a
  temporary `TMUX_TMPDIR`, and runs `tmag-flow` as a person in its windows.
  Never point a test at the developer's tmux server, agents or tmag state.
- The fake agent answers each prompt with `answered: ` and the prompt's last
  line. Its templates give each start a session id of its own; `FAKE_SLOW`
  names the agents whose turns last a minute, which `/end` typed into their
  pane ends.
- A run started by an agent is tested with a fake agent whose tool runs
  tmag-flow (`is the agent whose tool runs it`).
- `TMAG_FLOW_CLI` runs the end-to-end tests against an installed package.
- CI (`ci.yml`'s flow jobs) runs the end-to-end tests on Linux and macOS
  against the oldest tmag README names and tmag of the same commit, and once
  against the packed package.
