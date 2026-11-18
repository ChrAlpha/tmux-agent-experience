// What the page knows across the computers it works with, as signals the
// screens read: the computers, the route, one inbox of their sessions, and
// the clock. Each computer keeps what it knows of itself (computer.ts); the
// page keeps the computers, with their credentials, in this browser's
// storage.

import { batch, computed, effect, signal } from '@preact/signals'
import * as api from './api'
import { Computer, type Hooks, type Saved } from './computer'
import type { InfoDoc, Session } from './contract'
import { parse, type Route } from './route'
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
  // address, is forgotten: the one added last.
  identified: (c) => {
    const same = computers.value.includes(c) && twin(c)
    if (!same) return
    const later = computers.value.indexOf(c) > computers.value.indexOf(same) ? c : same
    forget(later, twice(later, later === c ? same : c))
  },
}

const made = (s: Saved) => new Computer(s, hooks)

/** The computers the page works with, in the order they were paired. */
export const computers = signal<readonly Computer[]>(saved().map(made))
/** Why the page forgot a computer, if it did. */
export const notice = signal('')
/** What became of a start the person left before it ended, and where to see it. */
export const later = signal<{ text: string; href?: string; label?: string } | null>(null)

// title names a computer: by the name it is proxied at, or the one that
// serves the page by the first label of its host.
export const title = (c: Computer) => c.name || location.hostname.split('.')[0] || 'this computer'

/** The computer a route names, if the page works with one by that name. */
export const named = (name: string) => computers.value.find((c) => c.name === name)

export const route = signal<Route>(parse(location.hash))

/** Where a link to a computer the page does not work with went: it goes on there once the page does. */
export const linked = signal<{ hash: string; name: string } | null>(null)

// Whether the page came to what it shows from the agents' list, the entry
// before it in the browser's history.
let fromList = false

function follow() {
  const next = parse(location.hash)
  fromList = next.name !== 'list' && route.value.name === 'list'
  route.value = next
}

// back returns to the agents' list: the entry the page came from, or one in
// place of a page opened from a link, so that the browser's own back and a
// swipe go where the person expects.
export function back() {
  if (fromList) history.back()
  else location.replace('#/')
}

/** The time "ago" counts from, a little behind. */
export const now = signal(Date.now())

/** A session, with the computer it runs on. */
export interface Entry {
  c: Computer
  s: Session
}

const rank: Record<string, number> = { blocked: 0, working: 1, idle: 2 }
const at = (s: Session) => (s.since ? Date.parse(s.since) : 0)

/** Every computer's sessions, blocked, working then idle, the latest first. */
export const inbox = computed<Entry[]>(() =>
  computers.value
    .flatMap((c) => sessions(c.view.value).map((s) => ({ c, s })))
    .sort((a, b) => (rank[a.s.state] ?? 3) - (rank[b.s.state] ?? 3) || at(b.s) - at(a.s)),
)

/** How many sessions wait on the person, or ended a turn unseen. */
export const waiting = computed(() =>
  inbox.value.reduce((n, { c, s }) => {
    const seen = c.seen.value
    return n + (s.state === 'blocked' || (seen && done(s, seen)) ? 1 : 0)
  }, 0),
)

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
  batch(() => {
    notice.value = ''
    computers.value = [...computers.value, c]
  })
  keep()
  const said = AbortSignal.timeout(5_000)
  const info = await api.get<InfoDoc>(base, credential.token, 'info', {}, said).catch(() => null)
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
