import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { parseDuration } from './duration.ts'
import { Abandoned, Failure, FlowError, type Halt, Stop, UsageError } from './errors.ts'
import { type Budget, Journal, layout } from './journal.ts'
import { acquire } from './lock.ts'
import { bundledPlans, findRun, hashFile, newRun, projectOf, resolvePlan, runs } from './project.ts'
import { Runtime } from './runtime.ts'
import { type Caller, type Doc, describe, exit, locate, nextOf, Tmag } from './tmag.ts'
import type { Flow } from './types.ts'
import { lockHolder, type RunView, render, schema, viewOf } from './view.ts'

export const code = { done: 0, error: 1, usage: 2, stopped: 4, halted: 5 } as const

const usage = `tmag-flow — plans written as code, run over tmag

Usage:
  tmag-flow start  <plan> [--run NAME] [--arg KEY=VALUE]... [--for D] [--concurrency N]
                   [--agents N] [--ask-timeout D] [--json]
                                      start a run of a plan, one that ships or a file, and advance it
  tmag-flow resume <run> [--for D] [--concurrency N] [--agents N] [--ask-timeout D] [--json]
                                      advance a run from its journal
  tmag-flow status [<run>] [--json]   a run's roles, steps and halts; the latest run by default
  tmag-flow decide <run> <step> <choice> [--note TEXT] [--json]
                                      decide a halted step; the next resume carries it out
  tmag-flow list   [--json]           the runs of this project
  tmag-flow plans  [--json]           the plans that ship with tmag-flow
  tmag-flow version

A run advances until its plan returns, a step halts for a decision, its
--for time is spent, or it is stopped. Runs live in .tmag-flow/ in the main
worktree of the repository tmag-flow runs in.

Exit codes: 0 done, 1 error, 2 usage, 4 stopped (time spent or a signal:
resume it), 5 halted (decide, then resume).
`

const defaults: Budget = { concurrency: 8, agents: 16, askTimeout: 3_600_000 }

interface Parsed {
  positional: string[]
  flags: Map<string, string[]>
}

const known: Record<string, { value: boolean; repeat?: boolean }> = {
  run: { value: true },
  arg: { value: true, repeat: true },
  for: { value: true },
  concurrency: { value: true },
  agents: { value: true },
  'ask-timeout': { value: true },
  note: { value: true },
  json: { value: false },
  help: { value: false },
}

function parse(argv: string[], allowed: string[]): Parsed {
  const positional: string[] = []
  const flags = new Map<string, string[]>()
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i] as string
    if (a === '--') {
      positional.push(...argv.slice(i + 1))
      break
    }
    if (!a.startsWith('--') || a === '-') {
      positional.push(a)
      continue
    }
    const eq = a.indexOf('=')
    const name = a.slice(2, eq < 0 ? undefined : eq)
    const spec = known[name]
    if (!spec || !allowed.includes(name))
      throw new UsageError(`unknown flag --${name}`, 'tmag-flow --help lists the flags')
    let value = 'true'
    if (spec.value) {
      if (eq >= 0) value = a.slice(eq + 1)
      else {
        const v = argv[++i]
        if (v === undefined) throw new UsageError(`--${name} needs a value`)
        value = v
      }
    } else if (eq >= 0) throw new UsageError(`--${name} takes no value`)
    const list = flags.get(name) ?? []
    if (list.length && !spec.repeat) throw new UsageError(`--${name} is given twice`)
    list.push(value)
    flags.set(name, list)
  }
  return { positional, flags }
}

const one = (p: Parsed, name: string) => p.flags.get(name)?.[0]

function count(p: Parsed, name: string, fallback: number): number {
  const v = one(p, name)
  if (v === undefined) return fallback
  const n = Number(v)
  if (!Number.isInteger(n) || n < 1)
    throw new UsageError(`--${name} is a whole number from 1, not ${JSON.stringify(v)}`)
  return n
}

function budgetOf(p: Parsed, base: Budget): Budget {
  const t = one(p, 'ask-timeout')
  return {
    concurrency: count(p, 'concurrency', base.concurrency),
    agents: count(p, 'agents', base.agents),
    askTimeout: t === undefined ? base.askTimeout : parseDuration(t, '--ask-timeout'),
  }
}

function argsOf(p: Parsed): Record<string, string> {
  const args: Record<string, string> = {}
  for (const kv of p.flags.get('arg') ?? []) {
    const eq = kv.indexOf('=')
    const k = eq < 0 ? kv : kv.slice(0, eq)
    if (eq < 0 || !/^[A-Za-z][A-Za-z0-9_-]*$/.test(k))
      throw new UsageError(`--arg takes KEY=VALUE, not ${JSON.stringify(kv)}`)
    if (k in args) throw new UsageError(`--arg ${k} is given twice`)
    args[k] = kv.slice(eq + 1)
  }
  return args
}

export function version(): string {
  let dir = dirname(fileURLToPath(import.meta.url))
  for (let i = 0; i < 4; i++) {
    const pkg = join(dir, 'package.json')
    if (existsSync(pkg)) {
      const p = JSON.parse(readFileSync(pkg, 'utf8')) as { name?: string; version?: string }
      if (p.name === 'tmag-flow') return p.version ?? 'unknown'
    }
    dir = dirname(dir)
  }
  return 'unknown'
}

export interface Io {
  out: (text: string) => void
  err: (text: string) => void
  cwd: string
}

// main runs one command and returns its exit code.
export async function main(argv: string[], io: Io): Promise<number> {
  const [cmd, ...rest] = argv
  try {
    switch (cmd) {
      case 'start':
        return await start(rest, io)
      case 'resume':
        return await resume(rest, io)
      case 'status':
        return await status(rest, io)
      case 'decide':
        return await decide(rest, io)
      case 'list':
        return list(rest, io)
      case 'plans':
        return await plans(rest, io)
      case 'version':
      case '--version':
        io.out(`tmag-flow ${version()} (${schema})\n`)
        return code.done
      case undefined:
      case 'help':
      case '--help':
      case '-h':
        io.out(usage)
        return code.done
      default:
        throw new UsageError(`unknown command ${JSON.stringify(cmd)}`, 'tmag-flow --help lists the commands')
    }
  } catch (e) {
    return fail(e, io, `tmag-flow ${cmd ?? ''}`.trim())
  }
}

function fail(e: unknown, io: Io, what: string): number {
  if (e instanceof Failure) {
    io.err(`${what}: ${e.message}\n`)
    io.err(`next: ${e.next ?? 'tmag-flow --help shows the commands and their flags'}\n`)
    return e instanceof UsageError ? code.usage : code.error
  }
  io.err(`${what}: ${e instanceof Error ? e.message : String(e)}\n`)
  io.err('next: this is a fault of tmag-flow: run the command again, and report it if it fails alike\n')
  return code.error
}

async function connect(): Promise<{ tmag: Tmag; version: string }> {
  const tmag = new Tmag(locate())
  const r = await tmag.run(['version'], { json: false })
  const text = (r.stdout + r.stderr).trim()
  const m = /^tmag (\S+) \((tmag\/v\d+)\)/.exec(text)
  if (r.code !== 0 || !m) {
    throw new FlowError(
      `${tmag.bin} is not a tmag tmag-flow can use: ${text || describe(r)}`,
      'install the tmux plugin tmux-agent-experience, or set TMAG_FLOW_TMAG to its bin/tmag',
    )
  }
  if (m[2] !== 'tmag/v2') {
    throw new FlowError(`tmag-flow speaks tmag/v2, and ${tmag.bin} speaks ${m[2]}`, 'update the tmux plugin')
  }
  const v = /^(\d+)\.(\d+)\.(\d+)/.exec(m[1] as string)
  if (v && Number(v[1]) === 0 && Number(v[2]) < 8) {
    throw new FlowError(`tmag-flow needs tmag 0.8.0 or later, and ${tmag.bin} is ${m[1]}`, 'update the tmux plugin')
  }
  return { tmag, version: m[1] as string }
}

async function start(argv: string[], io: Io): Promise<number> {
  const p = parse(argv, ['run', 'arg', 'for', 'concurrency', 'agents', 'ask-timeout', 'json'])
  if (p.positional.length !== 1)
    throw new UsageError('start takes one plan', 'tmag-flow plans lists the plans that ship')
  const project = projectOf(io.cwd)
  const plan = resolvePlan(p.positional[0] as string, io.cwd)
  const args = argsOf(p)
  const budget = budgetOf(p, defaults)
  const deadline = deadlineOf(p)
  const t = await connect()
  const { name, dir } = newRun(project, plan.name, one(p, 'run'))
  const j = Journal.open(dir)
  const lock = acquire(dir, `run ${name}`)
  try {
    j.append({
      t: 'run',
      layout,
      run: name,
      plan: plan.name,
      planPath: plan.path,
      planHash: plan.hash,
      args,
      project,
      flow: version(),
      budget,
    })
    io.err(`tmag-flow: run ${name} of plan ${plan.name}\n`)
    return await advance(j, dir, t, budget, deadline, p.flags.has('json'), io)
  } finally {
    lock.release()
    j.close()
  }
}

async function resume(argv: string[], io: Io): Promise<number> {
  const p = parse(argv, ['for', 'concurrency', 'agents', 'ask-timeout', 'json'])
  if (p.positional.length !== 1) throw new UsageError('resume takes one run', 'tmag-flow list shows the runs')
  const project = projectOf(io.cwd)
  const dir = findRun(project, p.positional[0] as string)
  const lock = acquire(dir, `run ${p.positional[0]}`)
  const j = Journal.open(dir)
  try {
    const run = j.run
    if (!run || run.layout !== layout) {
      throw new FlowError(
        `${dir} holds no run tmag-flow ${version()} can resume`,
        'start the plan again as a new run with tmag-flow start',
      )
    }
    const end = j.ended()
    if (end) {
      io.err(`tmag-flow: run ${run.run} is done\n`)
      emit(io, p.flags.has('json'), { state: 'done', run: run.run, dir, result: end.result })
      return code.done
    }
    if (!existsSync(run.planPath)) {
      throw new FlowError(
        `the plan of run ${run.run} is gone: ${run.planPath}`,
        'put it back where it was, then resume',
      )
    }
    if (hashFile(run.planPath) !== run.planHash) {
      io.err(`tmag-flow: the plan changed since run ${run.run} started; its steps replay as far as they are the same\n`)
    }
    const t = await connect()
    return await advance(j, dir, t, budgetOf(p, run.budget), deadlineOf(p), p.flags.has('json'), io)
  } finally {
    lock.release()
    j.close()
  }
}

function deadlineOf(p: Parsed): number | undefined {
  const f = one(p, 'for')
  return f === undefined ? undefined : Date.now() + parseDuration(f, '--for')
}

async function callerOf(tmag: Tmag): Promise<Caller | null> {
  const r = await tmag.run(['list'])
  if (r.code !== exit.ok) {
    throw new FlowError(`tmag list: ${describe(r)}`, nextOf(r) ?? 'run the command again once tmag list runs')
  }
  const c = r.docs[0]?.caller as Caller | undefined
  return c ?? null
}

// advance runs the plan of a run until it returns, halts or stops.
async function advance(
  j: Journal,
  dir: string,
  t: { tmag: Tmag; version: string },
  budget: Budget,
  deadline: number | undefined,
  json: boolean,
  io: Io,
): Promise<number> {
  const run = j.run
  if (!run) throw new FlowError(`${dir} holds no run`, 'tmag-flow list shows the runs')
  const caller = await callerOf(t.tmag)
  j.append({ t: 'invocation', pid: process.pid, caller, tmag: t.version })
  const rt = new Runtime({
    journal: j,
    tmag: t.tmag,
    run: run.run,
    dir,
    project: run.project,
    args: run.args,
    budget,
    deadline,
    caller,
    startName: await t.tmag.hasStartName(),
    say: (line) => io.err(`tmag-flow: ${line}\n`),
  })
  const onSignal = () => {
    if (rt.stopReason === 'signal') process.exit(130)
    io.err('tmag-flow: stopping; the steps under way finish first (a second signal exits at once)\n')
    rt.stop('signal')
  }
  process.on('SIGINT', onSignal)
  process.on('SIGTERM', onSignal)
  let result: unknown
  let failure: unknown
  try {
    const mod = (await import(pathToFileURL(run.planPath).href)) as { default?: unknown }
    if (typeof mod.default !== 'function') {
      throw new UsageError(`${run.planPath} has no default export of a function that takes the flow`)
    }
    result = await (mod.default as (flow: Flow) => unknown)(rt.flow())
  } catch (e) {
    failure = e
  } finally {
    await rt.settle()
    process.off('SIGINT', onSignal)
    process.off('SIGTERM', onSignal)
  }

  if (rt.halts.length) {
    for (const h of rt.halts) {
      io.err(`tmag-flow: ${h.key} halted: ${h.question}\n`)
    }
    const h = rt.halts[0] as Halt
    const decide = `tmag-flow decide ${run.run} ${h.key} ${h.choices.join('|')}, then tmag-flow resume ${run.run}`
    io.err(`next: ${h.again ? `tmag-flow resume ${run.run} looks again; or ${decide}` : decide}\n`)
    emit(io, json, {
      state: 'halted',
      run: run.run,
      dir,
      halts: rt.halts.map((x) => ({ key: x.key, kind: x.kind, question: x.question, choices: x.choices })),
    })
    return code.halted
  }
  if (failure instanceof Stop || (failure !== undefined && rt.stopReason)) {
    const why = rt.stopReason === 'signal' ? 'it was stopped' : 'it spent its time'
    io.err(`tmag-flow: run ${run.run} paused: ${why}\n`)
    io.err(`next: tmag-flow resume ${run.run}\n`)
    emit(io, json, { state: 'stopped', run: run.run, dir })
    return code.stopped
  }
  if (failure !== undefined) {
    let message: string
    let next: string
    if (failure instanceof Abandoned) {
      message = `${failure.message}, and the plan does not handle it`
      next = `change the plan to catch the Abandoned error of ${failure.key}, then tmag-flow resume ${run.run}`
    } else if (failure instanceof Failure) {
      message = failure.message
      next = failure.next ?? `fix the plan, then tmag-flow resume ${run.run}: the steps it ran replay`
    } else {
      message = `the plan failed: ${failure instanceof Error ? failure.message : String(failure)}${where(failure)}`
      next = `fix the plan, then tmag-flow resume ${run.run}: the steps it ran replay. Its --arg values are the run's: other values need a new run`
    }
    j.append({ t: 'error', message: message.split('\n')[0] as string })
    io.err(`tmag-flow: ${message}\n`)
    io.err(`next: ${next}\n`)
    emit(io, json, { state: 'failed', run: run.run, dir, error: message.split('\n')[0] })
    return failure instanceof UsageError ? code.usage : code.error
  }
  const kept = result === undefined ? null : JSON.parse(JSON.stringify(result))
  j.append({ t: 'end', result: kept })
  io.err(`tmag-flow: run ${run.run} is done\n`)
  emit(io, json, { state: 'done', run: run.run, dir, result: kept }, true)
  return code.done
}

// where is the place in a plan an error was thrown, as its stack names it.
function where(e: unknown): string {
  const frame = (e instanceof Error ? (e.stack ?? '') : '')
    .split('\n')
    .slice(1)
    .find((l) => !l.includes('/src/runtime.') && !l.includes('node:'))
  const at = frame
    ?.trim()
    .replace(/^at (?:.* \()?(?:file:\/\/)?/, '')
    .replace(/\)$/, '')
  return at ? ` (at ${at})` : ''
}

function emit(io: Io, json: boolean, doc: Doc, printResult = false): void {
  if (json) {
    io.out(`${JSON.stringify({ schema, ...doc })}\n`)
    return
  }
  if (printResult && doc.result !== null && doc.result !== undefined) {
    io.out(typeof doc.result === 'string' ? `${doc.result}\n` : `${JSON.stringify(doc.result, null, 2)}\n`)
  }
}

async function liveSessions(): Promise<Map<string, Doc> | undefined> {
  try {
    const r = await new Tmag(locate()).run(['list'])
    if (r.code !== exit.ok) return undefined
    return new Map(((r.docs[0]?.sessions ?? []) as Doc[]).map((s) => [s.id as string, s]))
  } catch {
    return undefined
  }
}

async function status(argv: string[], io: Io): Promise<number> {
  const p = parse(argv, ['json'])
  if (p.positional.length > 1) throw new UsageError('status takes at most one run')
  const project = projectOf(io.cwd)
  let dir: string
  if (p.positional[0] !== undefined) dir = findRun(project, p.positional[0])
  else {
    const latest = runs(project)[0]
    if (!latest) throw new FlowError(`no runs in ${project}`, 'start one with tmag-flow start PLAN')
    dir = latest.dir
  }
  const v = viewOf(Journal.open(dir), dir, await liveSessions())
  io.out(p.flags.has('json') ? `${JSON.stringify(v)}\n` : `${render(v)}\n`)
  return code.done
}

async function decide(argv: string[], io: Io): Promise<number> {
  const p = parse(argv, ['note', 'json'])
  if (p.positional.length !== 3) {
    throw new UsageError(
      'decide takes a run, a step and a choice',
      'tmag-flow status RUN shows the halted steps and their choices',
    )
  }
  const [runArg, key, choice] = p.positional as [string, string, string]
  const project = projectOf(io.cwd)
  const dir = findRun(project, runArg)
  if (lockHolder(dir) !== undefined) {
    throw new FlowError(`run ${runArg} is advancing`, 'decide once it halts: tmag-flow status shows it')
  }
  const lock = acquire(dir, `run ${runArg}`)
  const j = Journal.open(dir)
  try {
    const h = j.halt(key)
    if (!h) {
      const halted = j.pending().map((x) => x.key)
      throw new UsageError(
        `step ${key} of run ${runArg} is not halted`,
        halted.length ? `the halted steps are ${halted.join(', ')}` : 'no step of this run is halted',
      )
    }
    if (!h.halt.choices.includes(choice)) {
      throw new UsageError(`${choice} is no choice of step ${key}: ${h.halt.choices.join(', ')}`)
    }
    if (h.used) throw new UsageError(`step ${key} already carried out its decision`, `tmag-flow resume ${runArg}`)
    let caller: Caller | null = null
    try {
      caller = await callerOf(new Tmag(locate()))
    } catch {
      // who decided stays unknown when tmag does not answer
    }
    const note = one(p, 'note')
    j.append({ t: 'decision', key, halt: h.halt.seq, choice, caller, ...(note ? { note } : {}) })
    io.err(`tmag-flow: decided ${key}: ${choice}\n`)
    io.err(`next: tmag-flow resume ${j.run?.run ?? runArg}\n`)
    if (p.flags.has('json')) io.out(`${JSON.stringify({ schema, run: j.run?.run, key, choice })}\n`)
    return code.done
  } finally {
    lock.release()
    j.close()
  }
}

function list(argv: string[], io: Io): number {
  const p = parse(argv, ['json'])
  if (p.positional.length) throw new UsageError('list takes no arguments')
  const project = projectOf(io.cwd)
  const views: RunView[] = runs(project).map((r) => viewOf(Journal.open(r.dir), r.dir))
  if (p.flags.has('json')) {
    io.out(
      `${JSON.stringify({ schema, project, runs: views.map((v) => ({ run: v.run, plan: v.plan, state: v.state, updated: v.updated, halts: v.halts.map((h) => h.key) })) })}\n`,
    )
    return code.done
  }
  if (!views.length) {
    io.out(`no runs in ${project}\n`)
    return code.done
  }
  const rows = [['RUN', 'PLAN', 'STATE', 'UPDATED'], ...views.map((v) => [v.run, v.plan, v.state, v.updated])]
  const w = rows[0]?.map((_, i) => Math.max(...rows.map((r) => (r[i] ?? '').length))) ?? []
  io.out(
    `${rows
      .map((r) =>
        r
          .map((c, i) => c.padEnd(w[i] ?? 0))
          .join('  ')
          .trimEnd(),
      )
      .join('\n')}\n`,
  )
  return code.done
}

async function plans(argv: string[], io: Io): Promise<number> {
  const p = parse(argv, ['json'])
  const out: { name: string; description: string }[] = []
  for (const name of bundledPlans()) {
    const plan = resolvePlan(name, io.cwd)
    const mod = (await import(pathToFileURL(plan.path).href)) as { description?: string }
    out.push({ name, description: mod.description ?? '' })
  }
  if (p.flags.has('json')) io.out(`${JSON.stringify({ schema, plans: out })}\n`)
  else io.out(`${out.map((x) => `${x.name.padEnd(10)}  ${x.description}`).join('\n')}\n`)
  return code.done
}
