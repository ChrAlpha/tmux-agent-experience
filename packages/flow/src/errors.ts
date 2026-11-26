// How an invocation ends before its plan returns, and the error a plan meets
// when a decision abandoned one of its steps.

// Failure is a command tmag-flow cannot carry out; next says what to do
// about it, as the line tmag-flow prints after it.
export abstract class Failure extends Error {
  readonly next: string | undefined
  constructor(message: string, next: string | undefined) {
    super(message)
    this.next = next
  }
}

// FlowError is a failure of the run or of what it runs on, which says what
// the person or agent running tmag-flow can do about it.
export class FlowError extends Failure {
  constructor(message: string, next: string) {
    super(message, next)
    this.name = 'FlowError'
  }
}

// UsageError is a command line, a plan or a plan's call that cannot work as
// written. Without a next of its own, what to do is in the command line's
// usage, or, in a run, in the plan.
export class UsageError extends Failure {
  constructor(message: string, next?: string) {
    super(message, next)
    this.name = 'UsageError'
  }
}

// Kinds of halt. A decision is the plan's own question; the others are the
// runner's: an outcome it cannot tell, an ask that outran its timeout (its
// turn, or other work before it), and a step the plan now describes
// differently from the one that ran.
export type HaltKind = 'decision' | 'unknown' | 'timeout' | 'divergence'

// The choices the runner offers for its own halts. It interrupts no turn:
// tmag interrupts a session, not a turn, so stopping one is for the person
// in its pane, before deciding.
export const choicesOf: Record<Exclude<HaltKind, 'decision'>, readonly string[]> = {
  unknown: ['rerun', 'abandon'],
  timeout: ['wait', 'abandon'],
  divergence: ['rerun'],
}

// Halt stops a step until someone decides it with tmag-flow decide; one a
// resume looks at again first, when what tmag shows may tell it.
export class Halt extends Error {
  readonly key: string
  readonly kind: HaltKind
  readonly question: string
  readonly choices: readonly string[]
  readonly again: boolean
  constructor(key: string, kind: HaltKind, question: string, choices: readonly string[], again = false) {
    super(`step ${key} halted: ${question}`)
    this.name = 'Halt'
    this.key = key
    this.kind = kind
    this.question = question
    this.choices = choices
    this.again = again
  }
}

// Stop ends an invocation that may resume: its time ran out, or it was told
// to stop.
export class Stop extends Error {
  readonly reason: 'budget' | 'signal'
  constructor(reason: 'budget' | 'signal') {
    super(reason === 'budget' ? 'the invocation spent its time' : 'the invocation was stopped')
    this.name = 'Stop'
    this.reason = reason
  }
}

// Abandoned is what a step throws into its plan once a decision abandoned it,
// on that run and on every replay. A plan may catch it by its name.
export class Abandoned extends Error {
  readonly key: string
  readonly reason: string
  constructor(key: string, reason: string) {
    super(`step ${key} was abandoned: ${reason}`)
    this.name = 'Abandoned'
    this.key = key
    this.reason = reason
  }
}
