import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, isAbsolute, join, relative, resolve } from 'node:path'
import { after, formatDuration, parseDuration } from './duration.ts'
import { Abandoned, choicesOf, FlowError, Halt, type HaltKind, Stop, UsageError } from './errors.ts'
import type { Budget, Journal, RoleSpec } from './journal.ts'
import { hash } from './project.ts'
import { type Caller, type Doc, describe, exit, nextOf, type Result, refusal, short, type Tmag } from './tmag.ts'
import type { Answer, AskOptions, Binding, CommandOptions, CommandResult, Flow, Role } from './types.ts'

// The runtime behind a plan's Flow, for one invocation. Every effect is a
// step with a key: the journal says whether it is done (and what it gave),
// started (and how far it got), or halted (and what was decided). A step
// that is done is replayed from the journal; one that was started picks up
// where the evidence says it is; one that cannot tell halts.

export interface Options {
  journal: Journal
  tmag: Tmag
  run: string
  dir: string
  project: string
  args: Record<string, string>
  budget: Budget
  // When this invocation stops starting work (epoch ms), if it has a limit.
  deadline: number | undefined
  caller: Caller | null
  startName: boolean
  say: (line: string) => void
}

// How long a delivery may take before tmag says whether it delivered; the
// wait for the turn follows as calls of its own.
const deliverWithin = '15s'
const waitChunk = 5 * 60_000
// How long one tmag start may take: at least what opening a window and a
// worktree needs, which no deadline cuts short, and at most a while, after
// which the runner waits for the pane itself and can stop.
const startAtLeast = 15_000
const startAtMost = 3 * 60_000
const commandTimeout = 10 * 60_000
const outputTail = 64 << 10
const maxFreeze = 1 << 20
// How many rounds an ask may take in one invocation before it gives up:
// every round that makes no progress waits a little longer.
const maxRounds = 40

const roleName = /^[a-z][a-z0-9_-]{0,23}$/
const stepKey = /^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$/
const choiceName = /^[a-z][a-z0-9-]{0,31}$/
const marker = '[tmag]'

// What a step was decided, and use, which records that the step carries
// the decision out: called right before an effect that is no record of the
// step's end (whose done settles its halt by itself), so that a decision is
// neither lost to an invocation that stopped first nor carried out twice.
type StepCtx = { decision: string | undefined; use: () => void }

export class Runtime {
  readonly o: Options
  readonly halts: Halt[] = []
  readonly controller = new AbortController()
  #stopReason: 'budget' | 'signal' | 'halt' | undefined
  #keys = new Set<string>()
  #roles = new Map<string, Role>()
  #binds = new Map<string, Promise<Binding>>()
  #inflight = new Set<Promise<unknown>>()
  #slots: Slots
  #timer: { clear(): void } | undefined
  #told = new Set<string>()

  constructor(o: Options) {
    this.o = o
    this.#slots = new Slots(o.budget.concurrency)
    if (o.deadline !== undefined) {
      this.#timer = after(o.deadline - Date.now(), () => this.stop('budget'))
    }
  }

  get signal(): AbortSignal {
    return this.controller.signal
  }

  get stopReason(): 'budget' | 'signal' | 'halt' | undefined {
    return this.#stopReason
  }

  // stop ends the invocation's work: no step starts, waits end, deliveries
  // under way finish. The run resumes from the journal.
  stop(reason: 'budget' | 'signal' | 'halt'): void {
    if (this.#stopReason) return
    this.#stopReason = reason
    this.#timer?.clear()
    this.controller.abort(reason)
  }

  // settle waits for the steps still under way once the plan returned or
  // threw, so that none is cut short between its effect and its record.
  async settle(): Promise<void> {
    this.#timer?.clear()
    while (this.#inflight.size) await Promise.allSettled([...this.#inflight])
  }

  flow(): Flow {
    const o = this.o
    return Object.freeze({
      run: o.run,
      dir: o.dir,
      project: o.project,
      args: Object.freeze({ ...o.args }),
      role: (name: string, spec: RoleSpec) => this.role(name, spec),
      bind: (role: Role) => this.bind(role),
      ask: (key: string, role: Role, prompt: string, options?: AskOptions) => this.ask(key, role, prompt, options),
      gate: (key: string, command: string, options?: CommandOptions) => this.command('gate', key, command, options),
      exec: (key: string, command: string, options?: CommandOptions) => this.command('exec', key, command, options),
      decide: (key: string, question: string, choices: readonly string[]) => this.decide(key, question, choices),
      freeze: (key: string, path: string) => this.freeze(key, path),
      path: (...segments: string[]) => this.path(...segments),
      log: (message: string) => o.say(message),
    })
  }

  // ---- roles ---------------------------------------------------------------

  role(name: string, spec: RoleSpec): Role {
    if (!roleName.test(name)) {
      throw new UsageError(
        `a role is named by up to 24 lowercase letters, digits, - and _, starting with a letter; not ${JSON.stringify(name)}`,
      )
    }
    if (typeof spec?.agent !== 'string' || !spec.agent) throw new UsageError(`role ${name} names no agent`)
    if (spec.dir !== undefined && spec.worktree !== undefined) {
      throw new UsageError(`role ${name} names a directory and a worktree: give one`)
    }
    const clean: RoleSpec = { agent: spec.agent }
    if (spec.dir !== undefined) clean.dir = spec.dir
    if (spec.worktree !== undefined) clean.worktree = spec.worktree
    const known = this.#roles.get(name)
    if (known) {
      if (JSON.stringify(known.spec) !== JSON.stringify(clean)) {
        throw new UsageError(`role ${name} is declared twice, differently`)
      }
      return known
    }
    const started = this.o.journal.role(name).intent?.spec
    if (started && JSON.stringify(started) !== JSON.stringify(clean)) {
      throw new UsageError(
        `role ${name} started as ${JSON.stringify(started)}, and the plan now declares ${JSON.stringify(clean)}`,
        'start a new run for the changed plan',
      )
    }
    const role = Object.freeze({ name, spec: Object.freeze(clean) })
    this.#roles.set(name, role)
    return role
  }

  #own(role: Role): Role {
    if (this.#roles.get(role?.name) !== role) throw new UsageError("a role must come from this flow's role()")
    return role
  }

  // bind gives the role a session that runs, one bind at a time per role.
  // An agent that shows no session yet is waited for within the time an ask
  // may take, from when its start began.
  bind(role: Role, within = this.o.budget.askTimeout): Promise<Binding> {
    this.#own(role)
    const known = this.#binds.get(role.name)
    if (known) return known
    const p = this.#track(this.#bind(role, within)).finally(() => this.#binds.delete(role.name))
    this.#binds.set(role.name, p)
    return p
  }

  async #bind(role: Role, within: number): Promise<Binding> {
    const j = this.o.journal
    for (;;) {
      this.#checkStop()
      const st = j.role(role.name)
      if (st.bound && !st.gone) {
        if (await this.#alive(st.bound.session)) {
          const b: Binding = { session: st.bound.session, agent: st.bound.agent, dir: st.bound.dir }
          return st.bound.window ? { ...b, window: st.bound.window } : b
        }
        j.append({ t: 'role.gone', role: role.name, gen: st.gen, session: st.bound.session })
        this.o.say(`${role.name}: session ${short(st.bound.session)} ended; another starts`)
        continue
      }
      if (st.pending && !st.gone) {
        const until = Date.parse((st.intent ?? st.pending).at) + within
        if (!(await this.#awaitPane(role, st.gen, st.pending.pane, until))) {
          j.append({ t: 'role.gone', role: role.name, gen: st.gen, session: '' })
        }
        continue
      }
      if (st.intent && !st.bound && !st.gone) {
        // A start no record finished: which session it opened, if any, no
        // record proves, so another starts.
        j.append({ t: 'role.gone', role: role.name, gen: st.gen, session: '' })
        this.o.say(
          `${role.name}: a start was cut short; an agent it opened may wait in a window of its own, which this run leaves alone`,
        )
        continue
      }
      if (st.started >= this.o.budget.agents) {
        throw new FlowError(
          `the run has started ${st.started} sessions, its limit`,
          `resume it with --agents ${st.started * 2} to let it start more`,
        )
      }
      await this.#start(role, st.gone || st.gen === 0 ? st.gen + 1 : st.gen, Date.now() + within)
    }
  }

  async #start(role: Role, gen: number, until: number): Promise<void> {
    const j = this.o.journal
    const spec = role.spec
    // Recorded before anything is awaited: the record is what counts the
    // run's sessions against its limit.
    j.append({ t: 'role.intent', role: role.name, gen, spec })
    const dir = spec.dir === undefined ? this.o.project : resolve(this.o.project, spec.dir)
    const left = Math.min(until, this.o.deadline ?? until) - Date.now()
    const within = formatDuration(Math.min(startAtMost, Math.max(startAtLeast, left)))
    const base = ['start', spec.agent, '--dir', dir, '--timeout', within]
    if (spec.worktree !== undefined) base.push('--worktree', spec.worktree)
    for (let n = 1; n <= 9; n++) {
      const name = this.o.startName ? windowName(role.name, this.o.run, n) : undefined
      const r = await this.o.tmag.run(name ? [...base, '--name', name] : base)
      const doc = r.docs[0] ?? {}
      if (r.code === exit.ok) {
        const s = doc.session as Doc
        j.append({
          t: 'role.bound',
          role: role.name,
          gen,
          session: s.id as string,
          agent: s.agent as string,
          dir: (doc.dir as string) ?? (s.cwd as string),
          ...(name ? { window: name } : {}),
        })
        this.o.say(`${role.name}: started ${s.agent} ${short(s.id as string)}${name ? ` in window ${name}` : ''}`)
        return
      }
      if (typeof doc.started === 'object' && doc.started) {
        await this.#record(role, gen, (doc.started as Doc).id as string, name)
        return
      }
      if (typeof doc.pane === 'object' && doc.pane) {
        const pane = doc.pane as { server: string; id: string }
        j.append({ t: 'role.pending', role: role.name, gen, pane: `${pane.server}:${pane.id}` })
        return
      }
      if (r.code === exit.usage && name && /named .* already/.test(r.message ?? '')) continue
      throw new FlowError(
        `starting role ${role.name}: ${describe(r)}`,
        nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag start ${spec.agent} works`,
      )
    }
    throw new FlowError(
      `starting role ${role.name}: windows named ${windowName(role.name, this.o.run, 1)} to -9 are all taken`,
      `close or rename those windows, then tmag-flow resume ${this.o.run}`,
    )
  }

  // #awaitPane waits until until for an agent whose window showed no session
  // yet, as at a trust dialog or a login, which the person answers there;
  // false when the pane is gone.
  async #awaitPane(role: Role, gen: number, pane: string, until: number): Promise<boolean> {
    if (!this.#told.has(`pane:${pane}`)) {
      this.#told.add(`pane:${pane}`)
      this.o.say(`${role.name}: its agent shows no session yet in pane ${pane}; the person answers what it shows there`)
    }
    for (;;) {
      this.#checkStop()
      // Looked at once at least, however late: the person may have answered.
      const left = Math.max(1000, Math.min(until - Date.now(), waitChunk))
      const r = await this.o.tmag.run(['wait', pane, '--for', 'idle,blocked', '--timeout', formatDuration(left)], {
        signal: this.signal,
      })
      this.#checkStop()
      if (r.code === exit.ok) {
        await this.#record(role, gen, r.docs[0]?.id as string)
        return true
      }
      if (r.error === 'not_found') return false
      if (r.code !== exit.timeout) {
        throw new FlowError(
          `${role.name}: waiting for pane ${pane}: ${describe(r)}`,
          nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag wait ${pane} works`,
        )
      }
      if (Date.now() >= until) {
        throw new FlowError(
          `${role.name}: its agent still shows no session in pane ${pane}`,
          `the person answers what pane ${pane} shows, then tmag-flow resume ${this.o.run}`,
        )
      }
    }
  }

  async #record(role: Role, gen: number, session: string, window?: string): Promise<void> {
    const r = await this.o.tmag.run(['inspect', session, '--events', '1'])
    const s = (r.docs[0]?.session ?? {}) as Doc
    this.o.journal.append({
      t: 'role.bound',
      role: role.name,
      gen,
      session,
      agent: (s.agent as string) ?? '',
      dir: (s.cwd as string) ?? '',
      ...(window ? { window } : {}),
    })
    this.o.say(`${role.name}: bound to ${s.agent ?? 'agent'} ${short(session)}`)
  }

  async #alive(session: string): Promise<boolean> {
    const r = await this.o.tmag.run(['inspect', session, '--events', '1'])
    if (r.code === exit.ok) return true
    if (r.error === 'ended' || r.error === 'not_found') return false
    throw new FlowError(
      `asking tmag about ${short(session)}: ${describe(r)}`,
      nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag inspect ${session} answers`,
    )
  }

  async #latest(session: string): Promise<number> {
    const r = await this.o.tmag.run(['inspect', session, '--events', '1'])
    const events = (r.docs[0]?.events ?? []) as Doc[]
    return (events.at(-1)?.seq as number) ?? 0
  }

  // ---- steps ---------------------------------------------------------------

  #claim(key: string, kind: string): void {
    if (typeof key !== 'string' || !stepKey.test(key)) {
      throw new UsageError(
        `a step key is up to 128 letters, digits and . _ : / -, starting with a letter or digit; not ${JSON.stringify(key)}`,
      )
    }
    if (this.#keys.has(key))
      throw new UsageError(`step ${key} is used twice in this run (by a ${kind}): each step needs a key of its own`)
    this.#keys.add(key)
  }

  #track<T>(p: Promise<T>): Promise<T> {
    this.#inflight.add(p)
    p.then(
      () => this.#inflight.delete(p),
      () => this.#inflight.delete(p),
    )
    return p
  }

  #checkStop(): void {
    if (!this.#stopReason) return
    throw new Stop(this.#stopReason === 'signal' ? 'signal' : 'budget')
  }

  // #halt records a halt and stops the invocation, which ends once the steps
  // under way settle.
  #halt(key: string, fp: string, kind: HaltKind, question: string, choices: readonly string[], again = false): Halt {
    this.o.journal.append({ t: 'halt', key, fp, kind, question, choices })
    const h = new Halt(key, kind, question, choices, again)
    this.halts.push(h)
    this.stop('halt')
    return h
  }

  #step<T>(
    key: string,
    kind: 'ask' | 'gate' | 'exec' | 'decide' | 'freeze',
    fp: string,
    body: (ctx: StepCtx) => Promise<T>,
  ): Promise<T> {
    this.#claim(key, kind)
    const j = this.o.journal
    const run = async (): Promise<T> => {
      const done = j.done(key)
      if (done?.fp === fp) return replay<T>(key, done.result)
      // A halt, and what was decided at it, is of the step as it was then:
      // one of a step the plan has changed since asks nothing of this one.
      const last = j.halt(key)
      const halted = last?.halt.fp === fp ? last : undefined
      // An ask that halted for want of evidence looks again: what tmag shows
      // may tell now.
      if (halted && !halted.decision && !(kind === 'ask' && halted.halt.kind === 'unknown')) {
        const h = new Halt(key, halted.halt.kind, halted.halt.question, halted.halt.choices)
        this.halts.push(h)
        this.stop('halt')
        throw h
      }
      // A step that ran, or started, as the plan no longer describes it is
      // another step: what it did or does is no outcome of this one.
      const ran = done ?? j.intent(key)
      if (ran && ran.fp !== fp && (halted?.halt.kind !== 'divergence' || halted.used)) {
        throw this.#halt(
          key,
          fp,
          'divergence',
          `the plan now describes this ${kind} differently from the one that ${done ? 'ran' : 'started'}`,
          choicesOf.divergence,
        )
      }
      this.#checkStop()
      const decided = halted?.decision && !halted.used ? halted.decision : undefined
      let used = false
      const use = () => {
        if (used || !decided) return
        used = true
        j.append({ t: 'use', key, decision: decided.seq })
      }
      // The reset carries the decision out by itself: what came before it,
      // the halt and its decision included, is no longer the step's.
      if (ran && ran.fp !== fp) {
        j.append({ t: 'reset', key })
        return body({ decision: undefined, use })
      }
      return body({ decision: decided?.choice, use })
    }
    return this.#track(run())
  }

  #finish(key: string, kind: 'ask' | 'gate' | 'exec' | 'decide' | 'freeze', fp: string, result: unknown): void {
    this.o.journal.append({ t: 'done', key, kind, fp, result })
  }

  #abandon(key: string, kind: 'ask' | 'gate' | 'exec', fp: string, reason: string): never {
    this.#finish(key, kind, fp, { abandoned: reason })
    throw new Abandoned(key, reason)
  }

  // ---- ask -----------------------------------------------------------------

  ask(key: string, role: Role, prompt: string, options: AskOptions = {}): Promise<Answer> {
    this.#own(role)
    if (typeof prompt !== 'string' || !prompt.trim()) throw new UsageError(`step ${key} asks nothing`)
    if (prompt.trimStart().toLowerCase().startsWith(marker)) {
      throw new UsageError(`step ${key}: a prompt may not start with ${marker}, which only tmag writes`)
    }
    const timeout =
      options.timeout === undefined ? this.o.budget.askTimeout : parseDuration(options.timeout, "an ask's timeout")
    const fp = hash('ask', role.name, prompt)
    return this.#step(key, 'ask', fp, async (ctx) => {
      await this.#slots.take(this.signal)
      try {
        return await this.#ask(key, role, prompt, fp, timeout, ctx)
      } finally {
        this.#slots.give()
      }
    })
  }

  async #ask(key: string, role: Role, prompt: string, fp: string, timeout: number, ctx: StepCtx): Promise<Answer> {
    const j = this.o.journal
    let intent = j.intent(key)
    if (ctx.decision === 'abandon') this.#abandon(key, 'ask', fp, 'decided')
    let state: 'deliver' | 'check' | 'resolve' = 'check'
    // A turn that outlived its timeout was delivered; other work that kept
    // the session busy as long was not, and the prompt goes once it is free.
    if (ctx.decision === 'wait') {
      ctx.use()
      if (intent && !j.delivered(key, intent.attempt)) state = 'deliver'
    }
    let fresh = ctx.decision === 'rerun'
    // The turn may take timeout from its prompt, and a busy session as long
    // to take it, or from the decision that gave either more time, however
    // many invocations it takes.
    let until = intent ? this.#since(key, intent) + timeout : Number.POSITIVE_INFINITY
    // The send that failed, when state is check because of it: evidence that
    // it delivered nothing ends the ask, since sending again fails alike.
    let failed: Result | undefined
    for (let round = 1; ; round++) {
      this.#checkStop()
      if (round > maxRounds) {
        throw new FlowError(
          `${key}: tmag-flow could not give ${role.name} the prompt after ${maxRounds} tries${failed ? `: ${describe(failed)}` : ''}`,
          `tmag-flow resume ${this.o.run} once tmag send reaches ${role.name}`,
        )
      }
      if (round > 1) await pause(Math.min(5000, 250 * round), this.signal)
      this.#checkStop()
      const ended = intent?.session !== undefined && !(await this.#alive(intent.session))
      if (fresh || !intent?.session || ended) {
        if (intent?.session && ended) {
          // A prompt its session may have been given is work it may have
          // done: only one it surely was not given is given to another.
          const seen = fresh
            ? 'absent'
            : j.delivered(key, intent.attempt)
              ? 'delivered'
              : await this.#evidence(key, intent)
          if (seen !== 'absent') {
            throw this.#halt(
              key,
              fp,
              'unknown',
              seen === 'delivered'
                ? `${role.name}'s session ${short(intent.session)} ended after it was given this prompt, before it answered`
                : `${role.name}'s session ${short(intent.session)} ended, and whether it was given this prompt cannot be told`,
              choicesOf.unknown,
              true,
            )
          }
          const st = j.role(role.name)
          if (st.bound?.session === intent.session && !st.gone) {
            j.append({ t: 'role.gone', role: role.name, gen: st.gen, session: intent.session })
          }
        }
        failed = undefined
        const b = await this.bind(role, timeout)
        const before = await this.#latest(b.session)
        this.#checkStop()
        // Each prompt has a line of its own at its top, so that only its
        // own turn is ever taken for it.
        const attempt = j.attempts(key) + 1
        ctx.use()
        fresh = false
        intent = j.append({
          t: 'intent',
          key,
          kind: 'ask',
          fp,
          attempt,
          session: b.session,
          tag: `tmag-flow run ${this.o.run}, step ${key}, attempt ${attempt}`,
          before,
        })
        until = Date.parse(intent.at) + timeout
        state = 'deliver'
      } else if (state === 'check' && j.delivered(key, intent.attempt)) {
        state = 'resolve'
      }
      const session = intent.session as string
      const tag = intent.tag as string

      if (state === 'check') {
        const seen = await this.#evidence(key, intent)
        if (seen === 'delivered') {
          j.append({ t: 'delivered', key, attempt: intent.attempt, session })
          until = this.#since(key, intent) + timeout
          state = 'resolve'
        } else if (j.open(key, intent.attempt)) {
          // Its runner stopped while it ran: until it shows, it may yet.
          throw this.#halt(
            key,
            fp,
            'unknown',
            `a send of this prompt to ${role.name} (${short(session)}) was under way when its runner stopped, and tmag cannot show yet that it delivered`,
            choicesOf.unknown,
            true,
          )
        } else if (seen === 'absent' && failed) {
          throw new FlowError(
            `${key}: tmag could not give ${role.name} the prompt: ${describe(failed)}`,
            nextOf(failed) ??
              `tmag-flow resume ${this.o.run} once tmag send reaches ${role.name}: the prompt is given again only where tmag shows it was not`,
          )
        } else if (seen === 'absent') {
          state = 'deliver' // a crash cut the step short before it delivered
        } else {
          throw this.#halt(
            key,
            fp,
            'unknown',
            `whether ${role.name} (${short(session)}) was given this prompt cannot be told`,
            choicesOf.unknown,
            true,
          )
        }
      }

      if (state === 'deliver') {
        this.o.say(`${key}: asking ${role.name} (${short(session)})`)
        // Recorded before its process starts, and its end after: a send no
        // record ended may still deliver.
        j.append({ t: 'send', key, attempt: intent.attempt })
        let r: Result | undefined
        try {
          r = await this.o.tmag.run(['send', session, '--wait', '--timeout', deliverWithin, '-'], {
            stdin: `${tag}\n${prompt}`,
          })
        } finally {
          j.append({ t: 'send.end', key, attempt: intent.attempt, code: r?.code ?? -1 })
        }
        const why = refusal(r)
        if (r.code === exit.ok || (r.code === exit.timeout && /delivered/.test(r.message ?? ''))) {
          j.append({ t: 'delivered', key, attempt: intent.attempt, session })
          until = this.#since(key, intent) + timeout
          state = 'resolve'
        } else if (why === 'busy' || why === 'owing' || why === 'subagent') {
          // Refused, so nothing was delivered: wait for the session to be free.
          if ((await this.#waitFor(session, 'idle', until)) === false) {
            throw this.#halt(
              key,
              fp,
              'timeout',
              `${role.name} (${short(session)}) stayed busy with other work for the whole timeout`,
              choicesOf.timeout,
            )
          }
          continue
        } else if (why !== undefined || r.code === exit.usage) {
          throw new FlowError(
            `${key}: tmag would not give ${role.name} the prompt: ${describe(r)}`,
            nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag send would`,
          )
        } else if ((r.error === 'ended' || r.error === 'not_found') && !(await this.#alive(session))) {
          continue // its session ended: the next round starts another
        } else {
          failed = r
          state = 'check'
          continue
        }
      }

      const got = await this.#resolve(role, session, tag, until)
      if (typeof got === 'object') {
        this.#finish(key, 'ask', fp, got)
        this.o.say(`${key}: ${role.name} answered`)
        return got
      }
      if (got === 'gone') continue
      if (got === 'lost') {
        throw this.#halt(
          key,
          fp,
          'unknown',
          `tmag sent ${role.name} (${short(session)}) the prompt, and no turn of its own shows it`,
          choicesOf.unknown,
          true,
        )
      }
      throw this.#halt(
        key,
        fp,
        'timeout',
        `${role.name} (${short(session)}) has not finished its turn within ${formatDuration(timeout)}`,
        choicesOf.timeout,
      )
    }
  }

  // #since is when an ask's time began to count: its intent until the
  // prompt is delivered, then its delivery, or a decision taken up since.
  #since(key: string, intent: { at: string; attempt: number }): number {
    const marks = this.o.journal
      .ofStep(key)
      .filter((r) => r.t === 'use' || (r.t === 'delivered' && r.attempt === intent.attempt))
    return Math.max(Date.parse(intent.at), ...marks.map((r) => Date.parse(r.at)))
  }

  // #evidence says whether an attempt's prompt was delivered: its tag in a
  // turn, or a Send after the intent by an invocation that sent it; absent
  // only when tmag shows the turns, and its events reach back to the intent
  // and show none.
  async #evidence(
    key: string,
    intent: { attempt: number; session?: string; tag?: string; before?: number },
  ): Promise<'delivered' | 'absent' | 'unknown'> {
    const session = intent.session as string
    const turns = await this.o.tmag.run(['read', session, '--turns', '20'])
    if (turns.code !== exit.ok) return 'unknown'
    if (findTurn((turns.docs[0]?.turns ?? []) as Doc[], intent.tag as string) >= 0) return 'delivered'
    const r = await this.o.tmag.run(['inspect', session, '--events', '64'])
    if (r.code !== exit.ok) return 'unknown'
    const events = (r.docs[0]?.events ?? []) as Doc[]
    const before = intent.before ?? 0
    const senders = this.o.journal.sends(key, intent.attempt).map((s) => this.#callerAt(s.seq))
    const sent = (e: Doc) => e.event === 'Send' && (e.seq as number) > before && senders.some((c) => mine(c, e))
    if (events.some(sent)) return 'delivered'
    // Absent only when the events tmag still holds reach back to before; a
    // log that rotated them away tells nothing.
    if (!events.length) return 'unknown'
    const oldest = Math.min(...events.map((e) => e.seq as number))
    return oldest <= before ? 'absent' : 'unknown'
  }

  // #callerAt is who ran the invocation that wrote the record at seq.
  #callerAt(seq: number): Caller | null {
    const inv = this.o.journal.all('invocation').findLast((r) => r.seq < seq)
    return inv?.caller ?? this.o.caller
  }

  // #resolve waits for the turn the tag names to end, and reads its reply.
  async #resolve(
    role: Role,
    session: string,
    tag: string,
    until: number,
  ): Promise<Answer | 'gone' | 'lost' | 'timeout'> {
    let idleWithout = 0
    for (;;) {
      this.#checkStop()
      const r = await this.o.tmag.run(['read', session, '--turns', '20'])
      if (r.code !== exit.ok) {
        if (r.error === 'ended' || r.error === 'not_found') return 'gone'
        throw new FlowError(
          `reading ${role.name}'s turns: ${describe(r)}`,
          nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag read ${session} --turns 20 works`,
        )
      }
      const s = (r.docs[0]?.session ?? {}) as Doc
      const turns = (r.docs[0]?.turns ?? []) as Doc[]
      const i = findTurn(turns, tag)
      const state = s.state as string
      if (i >= 0) {
        const turn = turns[i] as Doc
        if (i < turns.length - 1 || state === 'idle') {
          let text = (turn.reply as string) ?? ''
          if (i === turns.length - 1) {
            const last = await this.o.tmag.run(['read', session, '--last'])
            if (last.code === exit.ok && last.docs[0]?.turn === turn.id) text = (last.docs[0]?.text as string) ?? text
          }
          return { text, turn: turn.id as string, session }
        }
      } else if (state === 'idle') {
        // Delivered and not in a turn yet: the agent starts it in a moment.
        if (++idleWithout > 2) return 'lost'
        const w = await this.o.tmag.run(['wait', session, '--for', 'working,blocked', '--timeout', '5s'], {
          signal: this.signal,
        })
        this.#checkStop()
        if (w.error === 'ended' || w.error === 'not_found') return 'gone'
        continue
      }
      if (Date.now() >= until) return 'timeout'
      if (state === 'blocked' && !this.#told.has(`blocked:${session}:${s.turn}`)) {
        this.#told.add(`blocked:${session}:${s.turn}`)
        this.o.say(
          `${role.name} (${short(session)}) waits on a request for the person: tmag inspect ${session} shows it`,
        )
      }
      const ok = await this.#waitFor(session, state === 'blocked' ? 'idle' : 'idle,blocked', until)
      if (ok === 'gone') return 'gone'
    }
  }

  // #waitFor waits until the session reaches one of states, or until; false
  // when until came first, 'gone' when the session ended.
  async #waitFor(session: string, states: string, until: number): Promise<boolean | 'gone'> {
    for (;;) {
      this.#checkStop()
      const left = until - Date.now()
      if (left <= 0) return false
      const r = await this.o.tmag.run(
        ['wait', session, '--for', states, '--timeout', formatDuration(Math.min(left, waitChunk))],
        {
          signal: this.signal,
        },
      )
      this.#checkStop()
      if (r.code === exit.ok) return true
      if (r.error === 'ended' || r.error === 'not_found') return 'gone'
      if (r.code !== exit.timeout) {
        throw new FlowError(
          `waiting for ${short(session)}: ${describe(r)}`,
          nextOf(r) ?? `tmag-flow resume ${this.o.run} once tmag wait ${session} works`,
        )
      }
    }
  }

  // ---- commands ------------------------------------------------------------

  command(kind: 'gate' | 'exec', key: string, command: string, options: CommandOptions = {}): Promise<CommandResult> {
    if (typeof command !== 'string' || !command.trim()) throw new UsageError(`step ${key} runs no command`)
    const cwd = options.cwd === undefined ? this.o.project : resolve(this.o.project, options.cwd)
    const timeout =
      options.timeout === undefined ? commandTimeout : parseDuration(options.timeout, "a command's timeout")
    const fp = hash(kind, command, cwd)
    return this.#step(key, kind, fp, async (ctx) => {
      if (ctx.decision === 'abandon') this.#abandon(key, kind, fp, 'decided')
      const prior = this.o.journal.intent(key)
      if (prior && kind === 'exec' && ctx.decision !== 'rerun') {
        throw this.#halt(
          key,
          fp,
          'unknown',
          `the command was cut short, and what it did is unknown: ${command}`,
          choicesOf.unknown,
        )
      }
      if (!existsSync(cwd) || !statSync(cwd).isDirectory()) throw new UsageError(`step ${key}: no directory ${cwd}`)
      ctx.use()
      this.o.journal.append({ t: 'intent', key, kind, fp, attempt: this.o.journal.attempts(key) + 1 })
      this.o.say(`${key}: running ${command}`)
      const result = await runCommand(command, cwd, timeout, kind === 'gate' ? this.signal : undefined)
      if (kind === 'gate') this.#checkStop() // a gate cut short runs again on resume
      this.#finish(key, kind, fp, result)
      this.o.say(`${key}: ${result.ok ? 'passed' : `failed (exit ${result.code})`}`)
      return result
    })
  }

  // ---- decide and freeze ---------------------------------------------------

  decide(key: string, question: string, choices: readonly string[]): Promise<string> {
    if (typeof question !== 'string' || !question.trim()) throw new UsageError(`step ${key} asks no question`)
    if (!Array.isArray(choices) || choices.length === 0) throw new UsageError(`step ${key} offers no choice`)
    for (const c of choices) {
      if (typeof c !== 'string' || !choiceName.test(c)) {
        throw new UsageError(
          `step ${key}: a choice is up to 32 lowercase letters, digits and -, starting with a letter; not ${JSON.stringify(c)}`,
        )
      }
    }
    if (new Set(choices).size !== choices.length) throw new UsageError(`step ${key} offers a choice twice`)
    const fp = hash('decide', question, [...choices])
    return this.#step(key, 'decide', fp, async (ctx) => {
      if (ctx.decision !== undefined) {
        this.#finish(key, 'decide', fp, ctx.decision)
        return ctx.decision
      }
      throw this.#halt(key, fp, 'decision', question, choices)
    })
  }

  freeze(key: string, path: string): Promise<string> {
    const abs = resolve(this.o.project, path)
    const fp = hash('freeze', abs)
    return this.#step(key, 'freeze', fp, async () => {
      if (!existsSync(abs) || !statSync(abs).isFile()) throw new UsageError(`step ${key}: no file ${abs}`)
      if (statSync(abs).size > maxFreeze)
        throw new UsageError(`step ${key}: ${abs} is over 1 MiB; freeze a smaller file`)
      const content = readFileSync(abs, 'utf8')
      this.#finish(key, 'freeze', fp, content)
      return content
    })
  }

  path(...segments: string[]): string {
    const root = join(this.o.dir, 'artifacts')
    const target = resolve(root, ...segments)
    const rel = relative(root, target)
    if (!segments.length || rel === '' || rel.startsWith('..') || isAbsolute(rel)) {
      throw new UsageError(
        `flow.path names a file inside the run's artifacts, not ${JSON.stringify(segments.join('/'))}`,
      )
    }
    mkdirSync(dirname(target), { recursive: true })
    return target
  }
}

// replay returns what a done step gave, as it gave it.
function replay<T>(key: string, result: unknown): T {
  if (result && typeof result === 'object' && 'abandoned' in result) {
    throw new Abandoned(key, String((result as { abandoned: unknown }).abandoned))
  }
  return result as T
}

// findTurn is the index of the last turn one of whose prompts starts with
// tag, below the line tmag writes, -1 if none: a prompt that only quotes it
// is someone else's.
function findTurn(turns: Doc[], tag: string): number {
  return turns.findLastIndex((t) =>
    ((t.prompts ?? []) as Doc[]).some(
      (p) =>
        String(p.text ?? '')
          .split('\n')
          .find((line) => !line.startsWith(marker))
          ?.trim() === tag,
    ),
  )
}

function mine(c: Caller | null, e: Doc): boolean {
  const by = e.by as { session?: string } | undefined
  if (c?.by) return by?.session === c.by.session
  const from = e.from as { id?: string; server?: string } | undefined
  return !by && !!c?.pane && from?.id === c.pane.id && from?.server === c.pane.server
}

// windowName names a role's window: the role, then letters of the run that
// are no hex digits, so tmag never takes the name for a session id.
export function windowName(role: string, run: string, n: number): string {
  const letters = 'ghjkmnpqrstvwxyz'
  const h = hash(run)
  const tag = [...h.slice(0, 4)].map((c) => letters[Number.parseInt(c, 16)]).join('')
  return `${role}-${tag}${n > 1 ? `-${n}` : ''}`.slice(0, 32)
}

// runCommand runs command in a shell of its own process group, killed at
// timeout or when signal aborts.
function runCommand(
  command: string,
  cwd: string,
  timeout: number,
  signal: AbortSignal | undefined,
): Promise<CommandResult> {
  return new Promise((done) => {
    const child = spawn('/bin/sh', ['-c', command], { cwd, detached: true, stdio: ['ignore', 'pipe', 'pipe'] })
    let out = Buffer.alloc(0)
    let cut = 0
    const keep = (b: Buffer) => {
      out = Buffer.concat([out, b])
      if (out.length > outputTail * 2) {
        cut += out.length - outputTail
        out = out.subarray(out.length - outputTail)
      }
    }
    child.stdout.on('data', keep)
    child.stderr.on('data', keep)
    let killed = false
    const kill = () => {
      killed = true
      try {
        process.kill(-(child.pid as number), 'SIGTERM')
        setTimeout(() => {
          try {
            process.kill(-(child.pid as number), 'SIGKILL')
          } catch {}
        }, 5000).unref()
      } catch {}
    }
    const timer = after(timeout, kill)
    signal?.addEventListener('abort', kill, { once: true })
    child.on('error', (e) => {
      timer.clear()
      done({ ok: false, code: -1, output: `could not run /bin/sh: ${e.message}` })
    })
    child.on('close', (code) => {
      timer.clear()
      signal?.removeEventListener('abort', kill)
      if (out.length > outputTail) {
        cut += out.length - outputTail
        out = out.subarray(out.length - outputTail)
      }
      let output = out.toString('utf8')
      if (cut) output = `[${cut} bytes cut]\n${output}`
      if (killed) output += `\n[killed${signal?.aborted ? '' : ` after ${formatDuration(timeout)}`}]`
      const c = killed || code === null ? -1 : code
      done({ ok: c === 0, code: c, output })
    })
  })
}

// pause waits ms, or less when signal aborts.
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((done) => {
    if (signal.aborted) return done()
    const t = setTimeout(finish, ms)
    function finish() {
      clearTimeout(t)
      signal.removeEventListener('abort', finish)
      done()
    }
    signal.addEventListener('abort', finish, { once: true })
  })
}

// Slots bound how many asks run at once.
class Slots {
  #free: number
  #queue: (() => void)[] = []
  constructor(n: number) {
    this.#free = n
  }
  take(signal: AbortSignal): Promise<void> {
    if (signal.aborted) return Promise.reject(new Stop(signal.reason === 'signal' ? 'signal' : 'budget'))
    if (this.#free > 0) {
      this.#free--
      return Promise.resolve()
    }
    return new Promise((ok, fail) => {
      const go = () => {
        signal.removeEventListener('abort', stop)
        ok()
      }
      const stop = () => {
        this.#queue = this.#queue.filter((f) => f !== go)
        fail(new Stop(signal.reason === 'signal' ? 'signal' : 'budget'))
      }
      signal.addEventListener('abort', stop, { once: true })
      this.#queue.push(go)
    })
  }
  give(): void {
    const next = this.#queue.shift()
    if (next) next()
    else this.#free++
  }
}
