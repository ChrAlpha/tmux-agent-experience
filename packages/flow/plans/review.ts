import { resolve } from 'node:path'
import type { Flow } from '../src/types.ts'
import { list, type Reviewed, reviewers, revisionsOf, round } from './lib/review.ts'

export const description =
  'reviewers check a directory in parallel; their findings are fixed and re-checked by who raised them'

// review: independent reviewers check a directory, each writing a report and
// a verdict. When any fails, the run halts for whoever fixes the findings;
// on recheck, those who failed check the fixes, until all pass or the last
// round fails.
//
//   --arg dir=PATH          what to review (required), relative to the project
//   --arg reviewers=A,B     agents or templates, one role each (required)
//   --arg brief=PATH        requirements the reviewers check against, frozen
//   --arg focus=TEXT        what every reviewer looks at first
//   --arg focus-NAME=TEXT   what reviewer NAME looks at first, after focus
//   --arg rounds=N          rounds before the run gives up (default 5)
//   --arg recheck=all       every reviewer checks each round, not only those
//                           that failed
export default async function review(flow: Flow) {
  const { args } = flow
  if (!args.dir) throw new Error('review needs --arg dir=PATH, the directory to review')
  const names = list(args.reviewers)
  if (!names.length) throw new Error('review needs --arg reviewers=A,B, the agents or templates that review')
  const rounds = Number(args.rounds ?? 5)
  if (!Number.isInteger(rounds) || rounds < 1) throw new Error('--arg rounds is a whole number from 1')
  const brief = args.brief ? await flow.freeze('brief', args.brief) : undefined
  const dir = resolve(flow.project, args.dir)
  const team = reviewers(flow, names, dir)

  let asked = team
  let previous = new Map<string, Reviewed>()
  let response: string | undefined
  const latest = new Map<string, Reviewed>()
  for (let n = 1; ; n++) {
    const got = await round(flow, asked, n, {
      dir,
      ...(brief ? { brief } : {}),
      focus: (r) => focusOf(args, r.name),
      previous,
      ...(response ? { response } : {}),
    })
    for (const r of got) latest.set(r.reviewer, r)
    const reviews = [...latest.values()]
    const failing = reviews.filter((r) => r.verdict !== 'pass')
    const revision = revisionsOf(reviews.map((r) => r.revision))
    const result = { passed: !failing.length, round: n, revision, reviews }
    if (result.passed || n === rounds) return result
    response = flow.path('responses', `round-${n}.md`)
    const reviewedDir = reviews.some((r) => r.revision !== undefined) ? `${dir} (${revision})` : dir
    const choice = await flow.decide(
      `round-${n}`,
      `${failing.map((r) => `${r.reviewer} (${r.verdict}): ${r.report ?? 'no report'}`).join('; ')}. Fix the findings in ${reviewedDir}, write what you fixed and why you left any finding as it is to ${response}, then recheck; or stop.`,
      ['recheck', 'stop'],
    )
    if (choice === 'stop') return result
    asked = args.recheck === 'all' ? team : team.filter((t) => failing.some((f) => f.reviewer === t.name))
    previous = new Map(asked.map((t) => [t.role.name, latest.get(t.name) as Reviewed]))
  }
}

// focusOf joins what every reviewer and what this one looks at first.
export function focusOf(args: Readonly<Record<string, string>>, name: string): string | undefined {
  const parts = [args.focus, args[`focus-${name}`]].filter((p): p is string => Boolean(p?.trim()))
  return parts.length ? parts.join('\n') : undefined
}
