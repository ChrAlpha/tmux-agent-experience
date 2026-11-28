import { Failure } from '../api'
import type { Action } from '../contract'

export type Verb = Action['verb']

// explain says what went wrong, and for a refused address how to allow it.
export function explain(e: unknown): string {
  if (e instanceof Failure && e.status === 403 && e.code === 'forbidden') {
    return `tmag serve does not accept this page's address. On the computer, set -g @tmag-serve '--host ${location.host} --origin ${location.origin}' and restart serve.`
  }
  return e instanceof Error ? e.message : String(e)
}

const not: Record<Verb, string> = {
  send: 'Not sent',
  interrupt: 'Not stopped',
  answer: 'Not answered',
  start: 'Not started',
  keys: 'Not typed',
}

// Why tmag refused a verb, by the tmag/v2 code it refused with, in what the
// person can do about it. A code missing here keeps tmag's own message.
const refusals: Record<string, string | Partial<Record<Verb, string>>> = {
  'refused:busy': {
    send: 'it cannot take a prompt while it works, or has not taken the last one yet. Send it again once it is idle.',
    interrupt: 'tmag sent it an interrupt moments ago, and it has not reported since.',
  },
  'refused:idle': 'it runs no turn now.',
  'refused:unbound': 'no tmux pane tmag can prove shows it, and no channel of its own reaches it.',
  'refused:background': 'its pane runs another program in the foreground.',
  'refused:subagent': 'its pane shows a subagent. Switch the pane back to the main agent.',
  'refused:changed': {
    send: 'it changed meanwhile.',
    interrupt: 'it changed meanwhile.',
    answer: 'it changed meanwhile. If it still asks, answer it in the terminal.',
    keys: 'the screen changed before it was typed. Look at it again.',
  },
  'refused:unsupported': {
    send: 'tmag has no way to prompt this session.',
    interrupt: 'tmag has no way to stop this session.',
    answer: 'tmag cannot answer this request. Answer it in the terminal.',
  },
  ended: 'the session has ended.',
  not_found: 'tmag no longer knows this session.',
}

// lost says whether the request failed without saying how the action ended.
const lost = (e: Failure) =>
  e.code === 'network' ||
  e.code === 'timeout' ||
  e.code === 'backend' ||
  e.code === 'receipt' ||
  (e.code === 'http' && e.status >= 500)

// refusal is why tmag refused a verb, in what the person can do about it,
// when it refused: a refused verb did nothing.
export function refusal(e: unknown, verb: Verb): string | undefined {
  if (!(e instanceof Failure) || !e.code.startsWith('refused:')) return undefined
  const why = refusals[e.code]
  return (typeof why === 'string' ? why : why?.[verb]) ?? e.message
}

// outcome tells what became of an action that failed: one whose answer was
// lost may or may not have happened; one tmag refused did not. tmag's own
// errors come after it acted, and may follow a delivery: what tmag says of
// one goes with it.
export function outcome(e: unknown, verb: Verb): string {
  if (e instanceof Failure) {
    if (lost(e))
      return 'The computer did not say how it went, so it may or may not have happened: check before trying again.'
    if (e.code === 'error')
      return `It may or may not have happened, so check before trying again. tmag says: ${e.message}`
    const why = refusals[e.code]
    const reason = typeof why === 'string' ? why : why?.[verb]
    if (reason) return `${not[verb]}: ${reason}`
  }
  return explain(e)
}

/** cls joins the class names that apply. */
export const cls = (...names: (string | false | null | undefined)[]) => names.filter(Boolean).join(' ')
