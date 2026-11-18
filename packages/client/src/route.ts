// Where the page is, as its address's hash says: the agents of every
// computer, a session of one, with a request of it, its start sheet, a pane
// of it, or pairing another computer. A computer is named in the hash by its
// name; the computer that serves the page by none.

export type Route =
  | { name: 'list' }
  | { name: 'add' }
  | { name: 'session'; at: string; id: string; request?: string }
  | { name: 'start'; at: string }
  | { name: 'pane'; at: string; pane: string }

/** What a computer may be named: the path it is proxied at, `/NAME/v2/`. */
export const computerName = /^[a-z0-9-]{1,32}$/

/** The words a hash uses, which no computer may be named. */
export const reserved = new Set(['s', 'r', 'p', 'start', 'add'])

export const nameable = (name: string) => computerName.test(name) && !reserved.has(name)

const decode = (part: string) => {
  try {
    return decodeURIComponent(part)
  } catch {
    return null
  }
}

// parse reads a hash; one it cannot read is the list.
export function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').map(decode)
  if (parts.some((p) => p === null)) return { name: 'list' }
  const words = parts as string[]
  if (words.length === 1 && words[0] === 'add') return { name: 'add' }
  const at = words[0] && nameable(words[0]) ? (words.shift() as string) : ''
  const [what, id, r, request, ...more] = words
  if (what === 's' && id) {
    if (r === undefined) return { name: 'session', at, id }
    if (r === 'r' && request && !more.length) return { name: 'session', at, id, request }
  }
  if (what === 'start' && id === undefined) return { name: 'start', at }
  if (what === 'p' && id && r === undefined) return { name: 'pane', at, pane: id }
  return { name: 'list' }
}

// hash writes a route as parse reads it.
export function hash(r: Route): string {
  switch (r.name) {
    case 'list':
      return '#/'
    case 'add':
      return '#/add'
    case 'session':
      return `#/${r.at ? `${r.at}/` : ''}s/${encodeURIComponent(r.id)}${r.request ? `/r/${encodeURIComponent(r.request)}` : ''}`
    case 'start':
      return `#/${r.at ? `${r.at}/` : ''}start`
    case 'pane':
      return `#/${r.at ? `${r.at}/` : ''}p/${encodeURIComponent(r.pane)}`
  }
}
