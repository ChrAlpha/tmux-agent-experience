// The view of one tmag instance that the page rebuilds from /v2/watch, as
// tmag/http/v2 tells a client to: a snapshot replaces sessions and panes, an
// event with a state reports its session in full, SessionEnd and Exit remove
// it, and a panes document replaces the panes. tmag's end-to-end follower
// (internal/e2e) keeps the same rules.

import type { Event, Pane, Session, Turn, WatchDoc } from './contract'

export interface View {
  /** Sessions by agent and id: two agents may share an id. */
  readonly sessions: ReadonlyMap<string, Session>
  readonly panes: readonly Pane[]
}

export const empty: View = { sessions: new Map(), panes: [] }

const key = (agent: string, id: string) => `${agent}/${id}`

// session is what a snapshot's entry or a state event says a session is; a
// field left out is empty.
function session(s: Partial<Session> & Pick<Session, 'id' | 'agent' | 'state'>): Session {
  return {
    id: s.id,
    agent: s.agent,
    state: s.state,
    kind: s.kind,
    turn: s.turn,
    since: s.since,
    cwd: s.cwd,
    runtime: s.runtime,
    capabilities: s.capabilities ?? [],
    requests: s.requests ?? [],
    subagents: s.subagents ?? [],
    owes: s.owes ?? [],
  }
}

// apply folds one watch document into a view. It returns the view itself
// when the document changes nothing, and otherwise a new view in which only
// the sessions the document reports are new objects, so that whoever holds
// one session can tell when it changed.
export function apply(view: View, doc: WatchDoc): View {
  switch (doc.type) {
    case 'snapshot': {
      const sessions = new Map<string, Session>()
      for (const s of doc.sessions ?? []) {
        if (s.id && s.agent && s.state)
          sessions.set(key(s.agent, s.id), session({ ...s, id: s.id, agent: s.agent, state: s.state }))
      }
      return { sessions, panes: doc.panes ?? [] }
    }
    case 'panes':
      return { ...view, panes: doc.panes ?? [] }
    case 'event':
      return event(view, doc.event)
  }
}

function event(view: View, e: Event): View {
  const k = key(e.agent, e.session)
  const old = view.sessions.get(k)
  if (e.event === 'SessionEnd' || e.event === 'Exit') {
    if (!old) return view
    const sessions = new Map(view.sessions)
    sessions.delete(k)
    return { ...view, sessions }
  }
  if (!e.state) return view
  // A state keeps the instant it began until its state or kind changes;
  // an event that knows when it began says so.
  const same = old && old.state === e.state && old.kind === e.kind
  const since = e.since ?? (same ? old.since : e.time)
  const sessions = new Map(view.sessions)
  sessions.set(k, session({ ...e, id: e.session, state: e.state, since }))
  return { ...view, sessions }
}

const rank: Record<string, number> = { blocked: 0, working: 1, idle: 2 }

// sessions lists what needs the person first: blocked, working, then idle,
// the latest change first within each.
export function sessions(view: View): Session[] {
  const at = (s: Session) => (s.since ? Date.parse(s.since) : 0)
  return [...view.sessions.values()].sort((a, b) => (rank[a.state] ?? 3) - (rank[b.state] ?? 3) || at(b) - at(a))
}

// find is the session with this id, whichever agent runs it.
export function find(view: View, id: string): Session | undefined {
  for (const s of view.sessions.values()) if (s.id === id) return s
  return undefined
}

// shared says whether more than one agent runs a session by this id: tmag
// names none of them by it, and neither does the page.
export const shared = (view: View, id: string) => [...view.sessions.values()].filter((s) => s.id === id).length > 1

// paneOf is the pane that shows the session, if one does.
export function paneOf(view: View, s: Session): Pane | undefined {
  return view.panes.find((p) => p.session === s.id && p.agent === s.agent)
}

// can says whether tmag can carry a control verb to the session.
export const can = (s: Session, verb: 'send' | 'interrupt' | 'answer') => s.capabilities.includes(verb)

// stoppable says whether the session runs a turn an interrupt can stop: tmag
// interrupts a session that waits on a prompt, or whose main agent works, not
// one that runs only background tasks.
export const stoppable = (s: Session) =>
  can(s, 'interrupt') && (s.state === 'blocked' || (s.state === 'working' && s.kind !== 'background'))

/**
 * A prompt the page sent, until the session's turns show it: how many of
 * their prompts had its text, and when the session's state began, as it was
 * sent.
 */
export interface Sent {
  /** Which of the prompts the page sent it is. */
  id: number
  text: string
  seen: number
  since: string | undefined
}

// follows says whether the page follows a prompt it sent into the turns: a
// slash command is the agent's to run, and starts no turn.
export const follows = (text: string) => !text.trim().startsWith('/')

// told counts the prompts of the turns that have this text.
export const told = (turns: readonly Turn[], text: string) =>
  turns.reduce((n, t) => n + t.prompts.filter((p) => p.text.trim() === text).length, 0)

// unread is what was sent to a session that its turns do not show yet. A
// prompt leaves once they show it, or once the session has gone idle since
// it was sent: its turns are written by then, in words the page may not
// recognize.
export const unread = (sent: readonly Sent[], turns: readonly Turn[], s: Session | undefined): Sent[] =>
  sent.filter((p) => told(turns, p.text) <= p.seen && !(s?.state === 'idle' && s.since !== p.since))

// unwritten says whether the turn an idle session last ended has no reply in
// its turns yet: an agent may write the reply after the hook that reported
// the end, as Claude Code may after its Stop hook.
export function unwritten(s: Session | undefined, turns: readonly Turn[]): boolean {
  const last = turns[turns.length - 1]
  return !!s?.turn && s.state === 'idle' && last?.id === s.turn && !last.reply
}

// helpers says how many subagents a session runs, if it runs any.
export function helpers(s: Session): string {
  const n = s.subagents.length
  return n ? `${n} subagent${n === 1 ? '' : 's'}` : ''
}

// place names a session by its working directory's last element.
export function place(s: Session): string {
  return s.cwd?.split('/').filter(Boolean).pop() ?? s.id.slice(0, 8)
}

// ago is how long ago an instant was, in its largest unit.
export function ago(iso: string | undefined, now: number): string {
  const s = Math.max(0, Math.round((now - Date.parse(iso ?? '')) / 1000))
  if (!Number.isFinite(s)) return ''
  if (s < 10) return 'now'
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

// status says what a session is doing, in a few words.
export function status(s: Session): string {
  switch (s.state) {
    case 'blocked':
      return s.kind === 'question'
        ? 'asks a question'
        : s.kind === 'plan'
          ? 'asks to approve a plan'
          : 'asks permission'
    case 'working':
      return s.kind === 'background' ? 'background tasks' : 'working'
    case 'idle':
      return s.kind === 'error' ? 'idle after an error' : 'idle'
    default:
      return s.state
  }
}

/**
 * What the person saw of each session of a computer: the turn it last ended
 * while they looked.
 */
export type Seen = Readonly<Record<string, string>>

// done says whether a session ended a turn the person has not seen it end.
export const done = (s: Session, seen: Seen) => s.state === 'idle' && !!s.turn && seen[s.id] !== s.turn

// look is what the person has seen once they look at a session: the turn it
// ended, if it is idle. A turn that runs while they look is seen once it
// ends with them still looking.
export function look(seen: Seen, s: Session): Seen {
  if (s.state !== 'idle' || !s.turn || seen[s.id] === s.turn) return seen
  return { ...seen, [s.id]: s.turn }
}

// firstSeen is what the person is taken to have seen of a computer the page
// has never shown: the turns its idle sessions ended. What runs then is done
// once it ends.
export function firstSeen(view: View): Seen {
  const seen: Record<string, string> = {}
  for (const s of view.sessions.values()) if (s.state === 'idle' && s.turn) seen[s.id] = s.turn
  return seen
}

// kept is what was seen of the sessions a view still shows.
export function kept(seen: Seen, view: View): Seen {
  const ids = new Set([...view.sessions.values()].map((s) => s.id))
  const left = Object.entries(seen).filter(([id]) => ids.has(id))
  return left.length === Object.keys(seen).length ? seen : Object.fromEntries(left)
}

/** Another session of the computer a session is tied to by an ask. */
export interface Tie {
  s: Session
  kind: 'final' | 'reply'
}

// workingFor is whom a session owes answers: the sessions that asked it, by
// what they asked for.
export function workingFor(view: View, s: Session): Tie[] {
  return s.owes.flatMap((o) => {
    const asker = o.to.session && view.sessions.get(key(o.to.agent, o.to.session))
    return asker ? [{ s: asker, kind: o.kind }] : []
  })
}

// waitsFor is whom a session asked and waits on: the sessions that owe it.
export function waitsFor(view: View, s: Session): Tie[] {
  return [...view.sessions.values()].flatMap((other) =>
    other.owes.filter((o) => o.to.session === s.id && o.to.agent === s.agent).map((o) => ({ s: other, kind: o.kind })),
  )
}

// ties says, in a few words each, how a session works with others: for whom,
// and on whom it waits.
export function ties(view: View, s: Session): { text: string; s: Session }[] {
  return [
    ...workingFor(view, s).map((t) => ({ text: t.kind === 'final' ? 'working for' : 'owes a reply to', s: t.s })),
    ...waitsFor(view, s).map((t) => ({ text: t.kind === 'final' ? 'waits for' : 'asked', s: t.s })),
  ]
}

const agentNames: Record<string, string> = { claude: 'Claude Code', codex: 'Codex', pi: 'Pi', omp: 'oh-my-pi' }

// agentName names an agent as its makers do.
export const agentName = (agent: string) => agentNames[agent] ?? agent
