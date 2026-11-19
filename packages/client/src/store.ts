// What the page knows across the computers it works with, as signals the
// screens read: the computers, the route and the way back, one inbox of
// their sessions and the same sessions by project, and the clock. Each computer keeps what it knows of itself (computer.ts); the
// page keeps the computers, with their credentials, in this browser's
// storage.

import { batch, computed, effect, signal } from '@preact/signals'
import * as api from './api'
import { Computer, type Hooks, type Saved } from './computer'
import type { Session } from './contract'
import { type Where, where } from './places'
import { type Home, hash, parse, type Route } from './route'
import { done, sessions } from './view'

const stored = 'tmag.computers'

// saved is what the page keeps of its computers. A page that kept one
// credential, before it worked with several computers, kept it for the one
// that serves it.
function saved(): Saved[] {
  try {
    const one = JSON.parse(localStorage.getItem('tmag.credential') ?? 'null') as Saved['credential'] | null
    if (one && typeof one.token === 'string' && !localStorage.getItem(stored)) {
      localStorage.setItem(stored, JSON.stringify([{ name: '', credential: one }]))
    }
    localStorage.removeItem('tmag.credential')
    const all = JSON.parse(localStorage.getItem(stored) ?? '[]') as Saved[]
    return Array.isArray(all)
      ? all.filter((c) => typeof c?.name === 'string' && typeof c.credential?.token === 'string')
      : []
  } catch {
    return []
  }
}

const keep = () => localStorage.setItem(stored, JSON.stringify(computers.value.map((c) => c.saved())))

// twin is another computer the page works with that is this instance.
const twin = (c: Computer, instance = c.instance) =>
  computers.value.find((x) => x !== c && !!instance && x.instance === instance)

const twice = (c: Computer, same: Computer) =>
  `That is ${title(same)}, which the page already works with. On it, revoke the credential just made: tmag auth revoke ${c.credential.id}`

const hooks: Hooks = {
  saved: () => keep(),
  // A computer that no longer accepts this device is forgotten.
  lost: (c, why) => forget(c, `The computer${c.name ? ` ${c.name}` : ''} ${why}`),
  // A computer that says it is one the page already works with, at another
  // address, is forgotten: the one added last. What the page showed at that
  // address it shows at the other.
  identified: (c) => {
    const same = computers.value.includes(c) && twin(c)
    if (!same) return
    const later = computers.value.indexOf(c) > computers.value.indexOf(same) ? c : same
    const kept = later === c ? same : c
    aliases.set(later.name, kept.name)
    const r = route.value
    const moved = 'at' in r && r.at === later.name ? { ...r, at: kept.name } : undefined
    batch(() => {
      // The route moves with the computer, so that no screen shows the
      // forgotten address in between; what the person wrote is the
      // instance's, at either address.
      if (moved) route.value = moved
      forget(later, twice(later, kept))
    })
    if (moved) go(moved, { replace: true })
  },
}

const made = (s: Saved) => new Computer(s, hooks)

/** The addresses the page forgot as another of a computer it kept, by the address it kept. */
export const aliases = new Map<string, string>()

// kept is the address the page keeps of one it may have forgotten as an alias.
export function kept(name: string): string {
  let at = name
  const seen = new Set<string>()
  while (aliases.has(at) && !seen.has(at)) {
    seen.add(at)
    at = aliases.get(at) as string
  }
  return at
}

/** The computers the page works with, in the order they were paired. */
export const computers = signal<readonly Computer[]>(saved().map(made))
/** Why the page forgot a computer, if it did. */
export const notice = signal('')
/**
 * What became of a start the person left before it ended, and where to see
 * it, on the computer instance it started on, at whichever address the page
 * keeps of it.
 */
export const later = signal<{ text: string; go?: Route; of?: Computer; instance?: string; label?: string } | null>(null)

// title names a computer: by the name it is proxied at, or the one that
// serves the page by the first label of its host.
export const title = (c: Computer) => c.name || location.hostname.split('.')[0] || 'this computer'

/** The instance a computer said it is, as it last said. */
export const instanceOf = (c: Computer) => (c.info.value ? c.info.value.instance : c.instance)

// holding is the computer the page works with that a computer was, as the
// instance it was then: itself while it is still that instance, else that
// instance at whichever address the page keeps of it; none for a computer
// that named no instance once the page let it go.
export function holding(instance: string | undefined, c: Computer): Computer | undefined {
  if (computers.value.includes(c) && instanceOf(c) === instance) return c
  return instance ? computers.value.find((x) => instanceOf(x) === instance) : undefined
}

/** The computer a route names, if the page works with one by that name. */
export const named = (name: string) => computers.value.find((c) => c.name === name)

export const route = signal<Route>(parse(location.hash))

const homeOf = (r: Route): Home | null => (r.name === 'inbox' || r.name === 'projects' ? r.name : null)

/** The view of every computer's sessions the person last had open. */
export const home = signal<Home>(homeOf(route.value) ?? 'inbox')

/** Where a link to a computer the page does not work with went: it goes on there once the page does. */
export const linked = signal<{ hash: string; name: string } | null>(null)

// Each entry the page makes in the browser's history carries how many of
// its entries come before it, and the address of the one before, so that
// the page's back returns to the entry the person came from, as the
// browser's own back and a swipe do, and goes up to their sessions from a
// page they opened from a link.
interface Mark {
  depth: number
  from?: string
}
const markOf = (state: unknown): Mark | undefined => {
  const m = state as Partial<Mark> | null
  return typeof m?.depth === 'number'
    ? { depth: m.depth, ...(typeof m.from === 'string' ? { from: m.from } : {}) }
    : undefined
}
const here = () => location.hash || '#/'
let mark: Mark = markOf(history.state) ?? { depth: 0 }
let address = here()
let replacing = false
const stamp = () => history.replaceState({ ...(history.state ?? {}), ...mark }, '')

function follow() {
  const known = markOf(history.state)
  if (known) mark = known
  else {
    // An entry in place of another keeps what came before that one.
    if (!replacing) mark = { depth: mark.depth + 1, from: address }
    stamp()
  }
  replacing = false
  address = here()
  route.value = parse(location.hash)
  home.value = homeOf(route.value) ?? home.value
}

// go shows a route: in an entry of its own, or in place of the one that
// shows, which back then skips; in place of one whose entry before shows
// the same, by going back to that one.
export function go(r: Route, { replace = false } = {}) {
  const to = hash(r)
  if (to === here()) return
  if (replace && mark.depth > 0 && mark.from === to) history.back()
  else if (!replace) location.hash = to
  else {
    replacing = true
    location.replace(to)
  }
}

// back returns to where the person came from, or to their sessions.
export function back() {
  if (mark.depth > 0) history.back()
  else go({ name: home.value }, { replace: true })
}

// tabKept is a draft this tab keeps under key while it holds something.
function tabKept(key: string) {
  const held = signal(sessionStorage.getItem(key) ?? '')
  effect(() => {
    if (held.value) sessionStorage.setItem(key, held.value)
    else sessionStorage.removeItem(key)
  })
  return held
}

/** The first prompt the person is writing for an agent they are about to start. */
export const startText = tabKept('tmag.draft.start')
/** How many times the person wrote in the first prompt: a start clears only the one it took. */
export const startEdits = signal(0)

/** The time "ago" counts from, a little behind. */
export const now = signal(Date.now())

/** A session, with the computer it runs on. */
export interface Entry {
  c: Computer
  s: Session
}

const rank: Record<string, number> = { blocked: 0, working: 1, idle: 2 }
const at = (s: Session) => (s.since ? Date.parse(s.since) : 0)

/** Whether a session waits on the person: it asks something, or ended a turn they have not seen end. */
export const needs = ({ c, s }: Entry) => {
  const seen = c.seen.value
  return s.state === 'blocked' || (!!seen && done(s, seen))
}

/** Every computer's sessions, blocked, working then idle, the latest first. */
export const inbox = computed<Entry[]>(() =>
  computers.value
    .flatMap((c) => sessions(c.view.value).map((s) => ({ c, s })))
    .sort((a, b) => (rank[a.s.state] ?? 3) - (rank[b.s.state] ?? 3) || at(b.s) - at(a.s)),
)

/** The inbox in parts: what waits on the person, what works, and the rest. */
export const sections = computed(() => {
  const waits: Entry[] = []
  const works: Entry[] = []
  const rest: Entry[] = []
  for (const e of inbox.value) (needs(e) ? waits : e.s.state === 'working' ? works : rest).push(e)
  return { waits, works, rest }
})

/** How many sessions wait on the person. */
export const waiting = computed(() => sections.value.waits.length)

// nextAfter is the session that waits on the person after this one, in the
// order the inbox shows them, from the first again after the last; the
// first, from one that does not wait.
export function nextAfter(c: Computer, id: string): Entry | undefined {
  const waits = sections.value.waits
  const at = waits.findIndex((e) => e.c === c && e.s.id === id)
  const next = waits[(at + 1) % waits.length]
  return next && next !== waits[at] ? next : undefined
}

// whereIs says where a session runs, in the repository its computer said
// its directory is in.
export const whereIs = ({ c, s }: Entry): Where | undefined => (s.cwd ? where(s.cwd, c.repoAt(s.cwd)) : undefined)

/** A project of a computer: the sessions that run in it. */
export interface Project {
  key: string
  c: Computer
  where: Where
  entries: Entry[]
}

/** The projects sessions run in, those with sessions that wait on the person first. */
export const projects = computed<Project[]>(() => {
  const by = new Map<string, Project>()
  for (const e of inbox.value) {
    const w = whereIs(e) ?? { root: '', name: e.s.id.slice(0, 8), worktree: false, sub: '' }
    const key = `${e.c.name}\u0000${w.root || e.s.id}`
    const p = by.get(key) ?? { key, c: e.c, where: { ...w, branch: undefined, worktree: false, sub: '' }, entries: [] }
    p.entries.push(e)
    by.set(key, p)
  }
  const urgency = (p: Project) => Math.min(...p.entries.map((e) => (needs(e) ? 0 : e.s.state === 'working' ? 1 : 2)))
  const first = (p: Project) => inbox.value.indexOf(p.entries[0] as Entry)
  return [...by.values()].sort((a, b) => urgency(a) - urgency(b) || first(a) - first(b))
})

// add pairs the page with a computer, by the code `tmag auth pair` printed
// there, at the name it is proxied at ('' for the one that serves the page).
// The credential is kept at once: the code is spent. The computer then shows
// nothing until it said which instance it is; one the page already works
// with, at another name, is not added twice, and the credential just made is
// the person's to revoke.
export async function add(name: string, code: string): Promise<Computer> {
  if (named(name)) throw new Error(`The page already works with ${name || 'this address'}.`)
  const base = name ? `/${name}` : ''
  const credential = await api.pair(base, code)
  const c = made({ name, credential })
  // The name is a computer's again, no other one's alias.
  aliases.delete(name)
  batch(() => {
    notice.value = ''
    computers.value = [...computers.value, c]
  })
  keep()
  const info = await c.ask(AbortSignal.timeout(5_000))
  const same = info && twin(c, info.instance)
  if (same) {
    forget(c)
    throw new Error(twice(c, same))
  }
  if (info) c.learn(info)
  void c.connect()
  return c
}

// forget drops a computer, and with it all the page read from it.
export function forget(c: Computer, why = '') {
  c.forget()
  batch(() => {
    computers.value = computers.value.filter((x) => x !== c)
    notice.value = why
  })
  keep()
}

// wake resumes every watch when the page shows again.
function wake() {
  if (document.hidden) return
  now.value = Date.now()
  for (const c of computers.value) c.wake()
}

// start follows the route, the watches and the clock while the page is open.
export function start() {
  stamp()
  addEventListener('hashchange', follow)
  document.addEventListener('visibilitychange', wake)
  addEventListener('online', wake)
  setInterval(() => {
    if (!document.hidden) now.value = Date.now()
  }, 15_000)
  effect(() => {
    document.title = `${waiting.value ? `(${waiting.value}) ` : ''}tmag`
  })
  for (const c of computers.value) void c.connect()
}
