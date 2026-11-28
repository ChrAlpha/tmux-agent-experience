import { closeSync, fsyncSync, openSync, readFileSync, truncateSync, writeSync } from 'node:fs'
import { join } from 'node:path'
import type { HaltKind } from './errors.ts'
import { FlowError } from './errors.ts'
import type { Caller } from './tmag.ts'

// A run's journal: one JSON record per line, appended and synced, never
// rewritten. It is the run's truth: a resumed run replays the steps it
// records as done and picks up the ones it records as started.

export const layout = 'tmag-flow/journal/v1'

export type StepKind = 'ask' | 'gate' | 'exec' | 'decide' | 'freeze'

export interface RoleSpec {
  agent: string
  dir?: string
  worktree?: string
}

export interface Budget {
  concurrency: number
  agents: number
  askTimeout: number
}

export type Entry =
  | {
      t: 'run'
      layout: string
      run: string
      plan: string
      planPath: string
      planHash: string
      args: Record<string, string>
      project: string
      flow: string
      budget: Budget
    }
  | { t: 'invocation'; pid: number; caller: Caller | null; tmag: string }
  | { t: 'role.intent'; role: string; gen: number; spec: RoleSpec }
  | { t: 'role.bound'; role: string; gen: number; session: string; agent: string; dir: string; window?: string }
  | { t: 'role.pending'; role: string; gen: number; pane: string }
  | { t: 'role.gone'; role: string; gen: number; session: string }
  | {
      t: 'intent'
      key: string
      kind: StepKind
      fp: string
      attempt: number
      session?: string
      tag?: string
      before?: number
    }
  | { t: 'send'; key: string; attempt: number; revision?: string }
  | { t: 'send.end'; key: string; attempt: number; code: number }
  | { t: 'delivered'; key: string; attempt: number; session: string }
  | { t: 'done'; key: string; kind: StepKind; fp: string; result: unknown }
  | { t: 'halt'; key: string; fp: string; kind: HaltKind; question: string; choices: readonly string[] }
  | { t: 'decision'; key: string; halt: number; choice: string; note?: string; caller: Caller | null }
  | { t: 'use'; key: string; decision: number }
  | { t: 'reset'; key: string }
  | { t: 'error'; message: string }
  | { t: 'end'; result: unknown }

export type Recorded = Entry & { seq: number; at: string }

type Of<T extends Entry['t']> = Extract<Recorded, { t: T }>

export class Journal {
  readonly path: string
  readonly records: Recorded[] = []
  #fd: number | undefined
  // The bytes of whole lines: a line a crash cut short after them is cut
  // off before the next append.
  #whole: number | undefined

  private constructor(path: string) {
    this.path = path
  }

  // open reads the journal of the run in dir; one that does not exist yet
  // is empty.
  static open(dir: string): Journal {
    const j = new Journal(join(dir, 'journal.jsonl'))
    let bytes = Buffer.alloc(0)
    try {
      bytes = readFileSync(j.path)
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e
    }
    // A line cut short by a crash is the last one, without its newline: it
    // never happened, and the next append cuts it off. Anything else that
    // does not parse is damage.
    const whole = bytes.lastIndexOf(0x0a) + 1
    if (whole < bytes.length) j.#whole = whole
    const lines = bytes.subarray(0, whole).toString('utf8').split('\n')
    lines.pop()
    for (const [i, line] of lines.entries()) {
      try {
        j.records.push(JSON.parse(line) as Recorded)
      } catch {
        throw new FlowError(
          `${j.path}: line ${i + 1} is not a journal record`,
          'the run cannot be resumed; start a new one',
        )
      }
    }
    return j
  }

  get run(): Of<'run'> | undefined {
    return this.records[0]?.t === 'run' ? (this.records[0] as Of<'run'>) : undefined
  }

  // append writes entry and syncs it before it returns.
  append<E extends Entry>(entry: E): E & { seq: number; at: string } {
    const record = { ...entry, seq: this.records.length, at: new Date().toISOString() } as Recorded
    if (this.#whole !== undefined) {
      truncateSync(this.path, this.#whole)
      this.#whole = undefined
    }
    this.#fd ??= openSync(this.path, 'a', 0o600)
    writeSync(this.#fd, `${JSON.stringify(record)}\n`)
    fsyncSync(this.#fd)
    this.records.push(record)
    return record as unknown as E & { seq: number; at: string }
  }

  close(): void {
    if (this.#fd !== undefined) closeSync(this.#fd)
    this.#fd = undefined
  }

  all<T extends Entry['t']>(t: T): Of<T>[] {
    return this.records.filter((r): r is Of<T> => r.t === t)
  }

  // ofStep is what the journal holds of a step since it was last reset.
  ofStep(key: string): Recorded[] {
    const mine = this.records.filter((r) => 'key' in r && r.key === key)
    const reset = mine.findLastIndex((r) => r.t === 'reset')
    return mine.slice(reset + 1)
  }

  done(key: string): Of<'done'> | undefined {
    return this.ofStep(key).findLast((r): r is Of<'done'> => r.t === 'done')
  }

  intent(key: string): Of<'intent'> | undefined {
    return this.ofStep(key).findLast((r): r is Of<'intent'> => r.t === 'intent')
  }

  // attempts is how many times the step was taken up, its resets apart.
  attempts(key: string): number {
    return this.records.filter((r) => r.t === 'intent' && r.key === key).length
  }

  // sends are the sends of an attempt, each recorded before its process
  // started.
  sends(key: string, attempt: number): Of<'send'>[] {
    return this.ofStep(key).filter((r): r is Of<'send'> => r.t === 'send' && r.attempt === attempt)
  }

  // open is an attempt's send that no record says ended or delivered: its
  // runner stopped first, and it may give the prompt yet.
  open(key: string, attempt: number): Of<'send'> | undefined {
    const send = this.sends(key, attempt).at(-1)
    if (!send) return undefined
    const settled = this.ofStep(key).some(
      (r) => (r.t === 'send.end' || r.t === 'delivered') && r.attempt === attempt && r.seq > send.seq,
    )
    return settled ? undefined : send
  }

  delivered(key: string, attempt: number): boolean {
    return this.ofStep(key).some((r) => r.t === 'delivered' && r.attempt === attempt)
  }

  // halt is the step's latest halt that no later record of the step's
  // completion settled, with the decision that answers it and whether the
  // step used that decision already.
  halt(key: string): { halt: Of<'halt'>; decision: Of<'decision'> | undefined; used: boolean } | undefined {
    const step = this.ofStep(key)
    const halt = step.findLast((r): r is Of<'halt'> => r.t === 'halt')
    if (!halt || step.some((r) => r.t === 'done' && r.seq > halt.seq)) return undefined
    const decision = step.findLast((r): r is Of<'decision'> => r.t === 'decision' && r.halt === halt.seq)
    const used = decision !== undefined && step.some((r) => r.t === 'use' && r.decision === decision.seq)
    return { halt, decision, used }
  }

  // pending are the halts nobody decided yet, in the order they happened.
  pending(): Of<'halt'>[] {
    const keys = new Set(this.all('halt').map((h) => h.key))
    const out: Of<'halt'>[] = []
    for (const key of keys) {
      const h = this.halt(key)
      if (h && !h.decision) out.push(h.halt)
    }
    return out.sort((a, b) => a.seq - b.seq)
  }

  // role is the latest generation of a role's binding.
  role(name: string): RoleState {
    const mine = this.records.filter(
      (r): r is Of<'role.intent'> | Of<'role.bound'> | Of<'role.pending'> | Of<'role.gone'> =>
        r.t.startsWith('role.') && (r as { role?: string }).role === name,
    )
    const gen = Math.max(0, ...mine.map((r) => r.gen))
    const now = mine.filter((r) => r.gen === gen)
    return {
      gen,
      intent: now.findLast((r): r is Of<'role.intent'> => r.t === 'role.intent'),
      bound: now.findLast((r): r is Of<'role.bound'> => r.t === 'role.bound'),
      pending: now.findLast((r): r is Of<'role.pending'> => r.t === 'role.pending'),
      gone: now.some((r) => r.t === 'role.gone'),
      started: this.all('role.intent').length,
    }
  }

  roles(): string[] {
    return [...new Set(this.records.flatMap((r) => (r.t.startsWith('role.') ? [(r as { role: string }).role] : [])))]
  }

  ended(): Of<'end'> | undefined {
    return this.records.findLast((r): r is Of<'end'> => r.t === 'end')
  }
}

export interface RoleState {
  gen: number
  intent: Of<'role.intent'> | undefined
  bound: Of<'role.bound'> | undefined
  pending: Of<'role.pending'> | undefined
  gone: boolean
  // How many sessions the run has started, for every role.
  started: number
}
