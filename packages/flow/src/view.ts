import type { Journal, StepKind } from './journal.ts'
import { holder } from './lock.ts'
import { type Caller, type Doc, short } from './tmag.ts'

// What a run is, read from its journal (and, when tmag answers, what its
// roles' sessions do now): what status and list show.

export const schema = 'tmag-flow/v1'

export type RunState = 'done' | 'advancing' | 'halted' | 'failed' | 'stopped'

export interface StepView {
  key: string
  kind: StepKind
  state: 'done' | 'abandoned' | 'halted' | 'decided' | 'started'
  attempts: number
  summary: string
}

export interface RoleView {
  name: string
  agent: string
  session: string | undefined
  dir: string | undefined
  window: string | undefined
  state: string | undefined
  sessions: number
}

export interface HaltView {
  key: string
  kind: string
  question: string
  choices: readonly string[]
}

export interface RunView {
  schema: typeof schema
  run: string
  plan: string
  planPath: string
  project: string
  dir: string
  state: RunState
  advancing: number | undefined
  created: string
  updated: string
  invocations: { at: string; pid: number; caller: Caller | null }[]
  roles: RoleView[]
  steps: StepView[]
  halts: HaltView[]
  error: string | undefined
  result: unknown
}

// lockHolder is the process that advances the run in dir, if one does.
export function lockHolder(dir: string): number | undefined {
  return holder(dir)?.pid
}

export function viewOf(j: Journal, dir: string, live?: Map<string, Doc>): RunView {
  const run = j.run
  if (!run) throw new Error(`${j.path} has no run record`)
  const last = j.records.at(-1)
  const end = j.ended()
  const halts = j.pending().map((h) => ({ key: h.key, kind: h.kind, question: h.question, choices: h.choices }))
  const holder = lockHolder(dir)
  const errors = j.all('error')
  const lastError = errors.at(-1)
  const lastInvocation = j.all('invocation').at(-1)
  const failed = lastError !== undefined && (lastInvocation === undefined || lastError.seq > lastInvocation.seq)
  let state: RunState = 'stopped'
  if (end) state = 'done'
  else if (holder !== undefined) state = 'advancing'
  else if (halts.length) state = 'halted'
  else if (failed) state = 'failed'

  const roles: RoleView[] = j.roles().map((name) => {
    const st = j.role(name)
    const bound = st.gone ? undefined : st.bound
    const sessions = j.all('role.bound').filter((r) => r.role === name).length
    const now = bound ? live?.get(bound.session) : undefined
    return {
      name,
      agent: st.intent?.spec.agent ?? '',
      session: bound?.session,
      dir: bound?.dir,
      window: bound?.window,
      state: bound ? (now ? (now.state as string) : live ? 'ended' : undefined) : st.pending ? 'starting' : undefined,
      sessions,
    }
  })

  const among = sessionsOf(roles)
  const keys: string[] = []
  for (const r of j.records) if ('key' in r && !keys.includes(r.key)) keys.push(r.key)
  const steps: StepView[] = []
  for (const key of keys) {
    const recs = j.ofStep(key)
    const intent = j.intent(key)
    const done = j.done(key)
    const h = j.halt(key)
    const kind = (done?.kind ?? intent?.kind ?? kindOfHalt(recs)) as StepKind | undefined
    if (!kind) continue
    let s: StepView['state'] = 'started'
    let summary = ''
    if (h && !h.decision) {
      s = 'halted'
      summary = h.halt.question
    } else if (h?.decision) {
      s = 'decided'
      summary = `decided ${h.decision.choice}${h.used ? '' : ', not carried out yet'}`
    } else if (done) {
      const res = done.result as Record<string, unknown> | string
      if (res && typeof res === 'object' && 'abandoned' in res) {
        s = 'abandoned'
        summary = `abandoned (${String(res.abandoned)})`
      } else {
        s = 'done'
        summary = summarize(kind, done.result)
      }
    } else if (intent) {
      summary = intent.session ? `under way with ${short(intent.session, among)}` : 'under way'
    }
    steps.push({ key, kind, state: s, attempts: intent?.attempt ?? 0, summary })
  }

  return {
    schema,
    run: run.run,
    plan: run.plan,
    planPath: run.planPath,
    project: run.project,
    dir,
    state,
    advancing: holder,
    created: run.at,
    updated: last?.at ?? run.at,
    invocations: j.all('invocation').map((i) => ({ at: i.at, pid: i.pid, caller: i.caller })),
    roles,
    steps,
    halts,
    error: failed ? lastError?.message : undefined,
    result: end?.result,
  }
}

function sessionsOf(roles: RoleView[]): string[] {
  return roles.flatMap((r) => (r.session ? [r.session] : []))
}

function kindOfHalt(recs: { t: string }[]): StepKind | undefined {
  return recs.some((r) => r.t === 'halt') ? 'decide' : undefined
}

function summarize(kind: StepKind, result: unknown): string {
  switch (kind) {
    case 'ask': {
      const a = result as { text?: string; turn?: string }
      return `answered in turn ${a.turn}: ${oneLine(a.text ?? '')}`
    }
    case 'gate':
    case 'exec': {
      const c = result as { ok?: boolean; code?: number }
      return c.ok ? 'passed' : `failed (exit ${c.code})`
    }
    case 'decide':
      return `decided ${String(result)}`
    case 'freeze':
      return `${String(result).length} characters kept`
  }
}

function oneLine(text: string): string {
  const line = text.trim().split('\n').at(-1) ?? ''
  return line.length > 80 ? `${line.slice(0, 77)}...` : line
}

export function who(c: Caller | null): string {
  if (c?.by) return `${c.by.agent} ${short(c.by.session)}`
  if (c?.pane) return `the person (pane ${c.pane.id})`
  return 'unknown'
}

// render writes a view for a person.
export function render(v: RunView): string {
  const out: string[] = []
  out.push(`run ${v.run}: ${v.state}${v.advancing ? ` (process ${v.advancing})` : ''}`)
  out.push(`plan ${v.plan} (${v.planPath})`)
  const first = v.invocations[0]
  const latest = v.invocations.at(-1)
  if (first) out.push(`started ${v.created} by ${who(first.caller)}`)
  if (latest && latest !== first) out.push(`last advanced ${latest.at} by ${who(latest.caller)}`)
  if (v.roles.length) {
    out.push('', 'roles:')
    const among = sessionsOf(v.roles)
    const rows = v.roles.map((r) => [
      r.name,
      r.agent,
      r.session ? short(r.session, among) : '-',
      r.state ?? '-',
      r.window ?? '-',
      r.dir ?? '-',
    ])
    out.push(...table([['ROLE', 'AGENT', 'SESSION', 'STATE', 'WINDOW', 'DIR'], ...rows]))
  }
  if (v.steps.length) {
    out.push('', 'steps:')
    out.push(
      ...table([['STEP', 'KIND', 'STATE', 'SUMMARY'], ...v.steps.map((s) => [s.key, s.kind, s.state, s.summary])]),
    )
  }
  for (const h of v.halts) {
    out.push('', `halted at ${h.key}: ${h.question}`, `  tmag-flow decide ${v.run} ${h.key} ${h.choices.join('|')}`)
  }
  if (v.error) out.push('', `last error: ${v.error}`)
  return out.join('\n')
}

function table(rows: string[][]): string[] {
  const widths = rows[0]?.map((_, i) => Math.max(...rows.map((r) => (r[i] ?? '').length))) ?? []
  return rows.map((r) =>
    r
      .map((c, i) => (i === r.length - 1 ? c : c.padEnd(widths[i] ?? 0)))
      .join('  ')
      .trimEnd(),
  )
}
