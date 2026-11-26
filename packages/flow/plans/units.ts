import { writeFileSync } from 'node:fs'
import type { CommandResult, Flow, Role } from '../src/types.ts'
import { list, type Reviewed, reviewers, round } from './lib/review.ts'

export const description =
  'an implementer does units in order, each gated by its acceptance commands, then reviewers check'

// units: one implementer works in a git worktree of its own through units,
// in order. After each unit the runner runs the unit's acceptance commands
// itself; a failure goes back to the implementer, and a unit that fails
// --arg attempts times halts for a decision. Once every unit passes,
// reviewers check the worktree; their findings go to the implementer, every
// acceptance command runs again as after a unit, and those who found
// something check again.
//
//   --arg units=PATH        the units, frozen when the run starts (required):
//                           {"units": [{"name": "parser", "task": "...",
//                           "accept": ["pnpm test parser"]}, ...]}
//   --arg implementer=A     agent or template that implements (required)
//   --arg branch=NAME       the implementer's worktree branch (required)
//   --arg reviewers=A,B     agents or templates that review at the end
//   --arg brief=PATH        the brief both read, frozen
//   --arg attempts=N        tries per unit before it halts (default 2)
//   --arg rounds=N          review rounds before the run gives up (default 3)

interface Unit {
  name: string
  task: string
  accept: string[]
}

const outputShown = 4000

export default async function units(flow: Flow) {
  const { args } = flow
  for (const need of ['units', 'implementer', 'branch']) {
    if (!args[need]) throw new Error(`units needs --arg ${need}`)
  }
  const attempts = whole(args.attempts ?? '2', 'attempts')
  const rounds = whole(args.rounds ?? '3', 'rounds')
  const plan = parseUnits(await flow.freeze('units', args.units as string))
  const brief = args.brief ? await flow.freeze('brief', args.brief) : undefined
  const impl = flow.role('impl', { agent: args.implementer as string, worktree: args.branch as string })

  const done: { unit: string; result: 'passed' | 'skipped'; tries: number }[] = []
  for (const [i, unit] of plan.entries()) {
    await flow.ask(`${unit.name}/task`, impl, task(unit, i + 1, plan.length, brief))
    const dir = (await flow.bind(impl)).dir
    const got = await accepted(flow, impl, unit.name, `unit ${unit.name}`, unit.accept, dir, attempts, [
      'retry',
      'skip',
      'stop',
    ])
    if (got.outcome === 'stop') return { stopped: unit.name, units: done }
    done.push({ unit: unit.name, result: got.outcome === 'skip' ? 'skipped' : 'passed', tries: got.tries })
  }

  const names = list(args.reviewers)
  if (!names.length) return { units: done }
  const dir = (await flow.bind(impl)).dir
  const team = reviewers(flow, names, dir)
  const latest = new Map<string, Reviewed>()
  let asked = team
  let previous = new Map<string, Reviewed>()
  let response: string | undefined
  for (let n = 1; n <= rounds; n++) {
    const reviewed = await round(flow, asked, n, {
      dir,
      ...(brief ? { brief } : {}),
      previous,
      ...(response ? { response } : {}),
    })
    for (const r of reviewed) latest.set(r.reviewer, r)
    const failing = [...latest.values()].filter((r) => r.verdict !== 'pass')
    if (!failing.length) return { units: done, review: { passed: true, round: n, reviews: [...latest.values()] } }
    if (n === rounds) break
    const fixed = await flow.ask(
      `review/fix-${n}`,
      impl,
      [
        'Reviewers found issues in your work. Read their reports and fix every finding that holds:',
        ...failing.map((f) => `- ${f.reviewer}: ${f.report ?? 'wrote no report, and reviews afresh'}`),
        'End your reply with what you fixed, and why you left any finding as it is.',
      ].join('\n'),
    )
    response = flow.path('responses', `round-${n}.md`)
    writeFileSync(response, fixed.text)
    const got = await accepted(
      flow,
      impl,
      `review/accept-${n}`,
      `the fixes of review round ${n}`,
      plan.flatMap((u) => u.accept),
      dir,
      attempts,
      ['retry', 'stop'],
    )
    if (got.outcome !== 'passed') {
      return { units: done, review: { passed: false, round: n, reviews: [...latest.values()] } }
    }
    asked = team.filter((t) => failing.some((f) => f.reviewer === t.name))
    previous = new Map(asked.map((t) => [t.role.name, latest.get(t.name) as Reviewed]))
  }
  return { units: done, review: { passed: false, round: rounds, reviews: [...latest.values()] } }
}

// accepted runs commands in dir until they all pass: each failure goes back
// to the implementer, and every attempts failures halt for a decision.
async function accepted(
  flow: Flow,
  impl: Role,
  at: string,
  what: string,
  commands: string[],
  dir: string,
  attempts: number,
  choices: ('retry' | 'skip' | 'stop')[],
): Promise<{ outcome: 'passed' | 'skip' | 'stop'; tries: number }> {
  for (let n = 1; ; n++) {
    const failed = await check(flow, `${at}/try-${n}`, commands, dir)
    if (!failed.length) return { outcome: 'passed', tries: n }
    if (n % attempts === 0) {
      const others = choices.filter((c) => c !== 'retry')
      const choice = (await flow.decide(
        `${at}/after-${n}`,
        `${what} failed its acceptance commands ${n} times (${failed.map((f) => f.command).join('; ')}). Fix it or narrow it, then retry; or ${others.map((c) => (c === 'skip' ? 'skip it' : c)).join(', or ')}.`,
        choices,
      )) as 'retry' | 'skip' | 'stop'
      if (choice !== 'retry') return { outcome: choice, tries: n }
    }
    await flow.ask(`${at}/fix-${n}`, impl, fix(what, failed))
  }
}

function whole(value: string, name: string): number {
  const n = Number(value)
  if (!Number.isInteger(n) || n < 1) throw new Error(`--arg ${name} is a whole number from 1`)
  return n
}

export function parseUnits(text: string): Unit[] {
  let doc: unknown
  try {
    doc = JSON.parse(text)
  } catch (e) {
    throw new Error(`the units file is not JSON: ${(e as Error).message}`)
  }
  const list = (doc as { units?: unknown })?.units
  if (!Array.isArray(list) || !list.length) throw new Error('the units file has no "units" list')
  const names = new Set<string>()
  return list.map((u, i) => {
    const unit = u as Partial<Unit>
    if (typeof unit.name !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,47}$/.test(unit.name)) {
      throw new Error(`unit ${i + 1} needs a "name" of letters, digits, ., _ and -`)
    }
    if (names.has(unit.name)) throw new Error(`unit ${unit.name} is named twice`)
    names.add(unit.name)
    if (typeof unit.task !== 'string' || !unit.task.trim()) throw new Error(`unit ${unit.name} needs a "task"`)
    if (
      !Array.isArray(unit.accept) ||
      !unit.accept.length ||
      !unit.accept.every((c) => typeof c === 'string' && c.trim())
    ) {
      throw new Error(`unit ${unit.name} needs "accept", a list of commands`)
    }
    return { name: unit.name, task: unit.task, accept: unit.accept }
  })
}

function task(unit: Unit, n: number, total: number, brief: string | undefined): string {
  return [
    `Unit ${n} of ${total}: ${unit.name}.`,
    unit.task,
    '',
    'When you are done, these acceptance commands check your work in your directory. Do not change them, or what they check:',
    ...unit.accept.map((c) => `  $ ${c}`),
    'End your reply with a short summary of what you changed.',
    brief ? `\nBrief:\n${brief}` : '',
  ]
    .join('\n')
    .trimEnd()
}

function fix(what: string, failed: { command: string; result: CommandResult }[]): string {
  return [
    `The acceptance commands of ${what} failed. Fix your work, not the commands:`,
    ...failed.flatMap((f) => [
      '',
      `$ ${f.command}  (exit ${f.result.code})`,
      f.result.output.length > outputShown ? `...\n${f.result.output.slice(-outputShown)}` : f.result.output,
    ]),
    '',
    'End your reply with what you changed.',
  ].join('\n')
}

async function check(flow: Flow, key: string, commands: string[], dir: string) {
  const failed: { command: string; result: CommandResult }[] = []
  for (const [i, command] of commands.entries()) {
    const result = await flow.gate(`${key}/accept-${i + 1}`, command, { cwd: dir })
    if (!result.ok) failed.push({ command, result })
  }
  return failed
}
