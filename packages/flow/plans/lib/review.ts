import type { Answer, Flow, Role } from '../../src/types.ts'

// What review and units share: reviewers, their prompts, and their verdicts.

export type Verdict = 'pass' | 'fail' | 'unclear'

export interface Reviewer {
  name: string
  role: Role
}

export interface Reviewed {
  reviewer: string
  verdict: Verdict
  // The report it wrote; none when it wrote none.
  report?: string
  // The session that answered; none when a decision abandoned the ask.
  session?: string
  // The revision recorded when that reviewer's prompt was sent.
  revision?: string
}

export const revisionUnavailable = 'revision unavailable'

export function revisionsOf(values: readonly (string | undefined)[]): string {
  const distinct = [...new Set(values.map((v) => v ?? revisionUnavailable))]
  return distinct.length > 1 ? `mixed (${distinct.join('; ')})` : (distinct[0] ?? revisionUnavailable)
}

// verdictOf reads the verdict a reply ends with, on a line of its own as
// the prompt asks: a verdict anywhere else is none.
export function verdictOf(text: string): Verdict {
  const last = text.trimEnd().split('\n').at(-1) ?? ''
  const m = /^\W*verdict\W*:?\W*(pass|fail)\W*$/i.exec(last.trim())
  return m ? ((m[1] as string).toLowerCase() as Verdict) : 'unclear'
}

// list reads a comma-separated argument.
export function list(value: string | undefined): string[] {
  return (value ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

// reviewers declares a role for each agent or template named, reviewing dir.
export function reviewers(flow: Flow, names: string[], dir: string): Reviewer[] {
  const seen = new Set<string>()
  return names.map((name) => {
    const role =
      name
        .toLowerCase()
        .replace(/[^a-z0-9_-]+/g, '-')
        .replace(/^[^a-z]+/, '') || 'reviewer'
    if (seen.has(role)) throw new Error(`reviewer ${name} is named twice`)
    seen.add(role)
    return { name, role: flow.role(role, { agent: name, dir }) }
  })
}

const verdictLine =
  'End your reply with your verdict on a line of its own: VERDICT: pass when nothing needs to change, VERDICT: fail when something does; put the report path on the line above it.'

export function firstReview(o: { dir: string; report: string; brief?: string; focus?: string }): string {
  return [
    `Review the work in ${o.dir} independently, as a reviewer who did not write it${o.brief ? ', against the brief below' : ''}.`,
    'Run what you need to check it, its build and tests included, but edit none of its files.',
    o.focus ? `Focus: ${o.focus}` : '',
    `Write your full report, each finding with its file and line and how to reproduce it, to ${o.report}.`,
    verdictLine,
    o.brief ? `\nBrief:\n${o.brief}` : '',
  ]
    .filter(Boolean)
    .join('\n')
}

export function recheck(o: { dir: string; report: string; previous: string; response?: string }): string {
  return [
    `Your previous report is ${o.previous}. Its findings were addressed in ${o.dir}.`,
    o.response
      ? `What was fixed, and why any finding was left as it is, is in ${o.response}. Check each fix, each reason against its finding, and what the fixes may have broken; edit none of its files.`
      : 'Check each fix, and what the fixes may have broken; edit none of its files.',
    `Write your report to ${o.report}.`,
    verdictLine,
  ].join('\n')
}

// round asks every reviewer in parallel: again, with the response to the
// findings when there is one, if it wrote its previous report, and first
// otherwise. An ask a decision abandoned is a review with no verdict.
export async function round(
  flow: Flow,
  who: Reviewer[],
  n: number,
  o: {
    dir: string
    brief?: string
    focus?: (r: Reviewer) => string | undefined
    previous?: Map<string, Reviewed>
    response?: string
  },
): Promise<Reviewed[]> {
  return Promise.all(
    who.map(async (r): Promise<Reviewed> => {
      const key = `review/${r.role.name}/round-${n}`
      const report = flow.path('reports', `${r.role.name}-round-${n}.md`)
      const previous = o.previous?.get(r.role.name)?.report
      const focus = o.focus?.(r)
      const prompt = previous
        ? recheck({ dir: o.dir, report, previous, response: o.response })
        : firstReview({ dir: o.dir, report, brief: o.brief, focus })
      let a: Answer
      try {
        a = await flow.ask(key, r.role, prompt, { revision: true })
      } catch (e) {
        if ((e as Error).name === 'Abandoned') return { reviewer: r.name, verdict: 'unclear' }
        throw e
      }
      // A verdict stands on the report it was asked for, as the turn left
      // it: the check is a step, so a resumed run judges the round alike.
      const wrote = (await flow.gate(`${key}/report`, `test -f ${quote(report)}`)).ok
      return {
        reviewer: r.name,
        verdict: wrote ? verdictOf(a.text) : 'unclear',
        ...(wrote ? { report } : {}),
        session: a.session,
        ...(a.revision ? { revision: a.revision } : {}),
      }
    }),
  )
}

// quote is s as one word of a shell command.
function quote(s: string): string {
  return `'${s.replaceAll("'", `'\\''`)}'`
}
