// Where the page is, as its address's hash says: the inbox of every
// computer's sessions or the same sessions by project, a session of one,
// with a request of it, the start sheet of one, in a directory, a pane of
// one, the computers this device works with, or pairing another. A
// computer is named in the hash by its name; the computer that serves the
// page by none.

export type Route =
  | { name: 'inbox' }
  | { name: 'projects' }
  | { name: 'settings' }
  | { name: 'add' }
  | { name: 'session'; at: string; id: string; request?: string }
  | { name: 'start'; at: string; dir?: string }
  | { name: 'pane'; at: string; pane: string }

/** The two views of every computer's sessions the page opens on. */
export type Home = 'inbox' | 'projects'

/** What a computer may be named: the path it is proxied at, `/NAME/v2/`. */
export const computerName = /^[a-z0-9-]{1,32}$/

/** The words that follow a computer's name in a hash, which none may be named. */
const words = new Set(['s', 'r', 'p', 'start'])

/** The words a hash uses, which no computer may be named from now on. */
export const reserved = new Set([...words, 'add', 'projects', 'settings'])

export const nameable = (name: string) => computerName.test(name) && !reserved.has(name)

// named says whether a hash's first word names a computer: a computer
// paired before a word became one a screen alone uses keeps its links.
const named = (word: string | undefined): word is string => !!word && computerName.test(word) && !words.has(word)

const decode = (part: string) => {
  try {
    return decodeURIComponent(part)
  } catch {
    return null
  }
}

// parse reads a hash; one it cannot read is the inbox.
export function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').map(decode)
  if (parts.some((p) => p === null)) return { name: 'inbox' }
  const all = parts as string[]
  if (all.length === 1 && (all[0] === 'add' || all[0] === 'projects' || all[0] === 'settings')) return { name: all[0] }
  const at = named(all[0]) ? (all.shift() as string) : ''
  const [what, id, r, request, ...more] = all
  if (what === 's' && id) {
    if (r === undefined) return { name: 'session', at, id }
    if (r === 'r' && request && !more.length) return { name: 'session', at, id, request }
  }
  if (what === 'start' && r === undefined) {
    if (id === undefined) return { name: 'start', at }
    if (id.startsWith('/')) return { name: 'start', at, dir: id }
  }
  if (what === 'p' && id && r === undefined) return { name: 'pane', at, pane: id }
  return { name: 'inbox' }
}

const on = (at: string) => (at ? `${at}/` : '')

// hash writes a route as parse reads it.
export function hash(r: Route): string {
  switch (r.name) {
    case 'inbox':
      return '#/'
    case 'projects':
    case 'settings':
    case 'add':
      return `#/${r.name}`
    case 'session':
      return `#/${on(r.at)}s/${encodeURIComponent(r.id)}${r.request ? `/r/${encodeURIComponent(r.request)}` : ''}`
    case 'start':
      return `#/${on(r.at)}start${r.dir ? `/${encodeURIComponent(r.dir)}` : ''}`
    case 'pane':
      return `#/${on(r.at)}p/${encodeURIComponent(r.pane)}`
  }
}
