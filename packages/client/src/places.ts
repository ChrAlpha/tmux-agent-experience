// Where sessions run, as the page shows and groups them: each session's
// project, the repository's main worktree where tmag says which repository
// its directory is in, or else the directory itself, and where in the
// project it runs; and a line or two of what an agent wrote, as plain text.

import type { Repo } from './contract'

/** Where a directory is, as the page names it. */
export interface Where {
  /** The project: the repository's main worktree, or the directory itself. */
  root: string
  /** The project's name: its root's last element. */
  name: string
  /** The branch its work tree has checked out, when tmag says so. */
  branch?: string
  /** Whether it is in a worktree other than the main one. */
  worktree: boolean
  /** Where in its work tree, or in its project, it is: '' at the top. */
  sub: string
}

const base = (path: string) => path.split('/').filter(Boolean).pop() ?? path

// inside is the path of dir under root, '' for root itself, or undefined
// when dir is not under root.
export function inside(dir: string, root: string): string | undefined {
  if (dir === root) return ''
  const prefix = root.endsWith('/') ? root : `${root}/`
  return dir.startsWith(prefix) ? dir.slice(prefix.length) : undefined
}

// where says where a directory is: in the repository tmag says it is in, or
// else a project of its own.
export function where(dir: string, repo?: Repo): Where {
  if (!repo) return { root: dir, name: base(dir), worktree: false, sub: '' }
  const root = repo.main ?? repo.top
  return {
    root,
    name: base(root),
    ...(repo.branch ? { branch: repo.branch } : {}),
    worktree: repo.top !== root,
    sub: inside(dir, repo.top) ?? '',
  }
}

// tilde writes a path under the home directory from ~.
export function tilde(path: string, home?: string): string {
  if (!home || home === '/') return path
  const under = inside(path, home)
  return under === undefined ? path : under ? `~/${under}` : '~'
}

// apart is, of paths that name alike, how each shows apart from the
// others in what a tile shows of it, two dozen columns: the fewest of its
// last directories that no other path ends in, whole where they fit, else
// the places where it parts from each other path, each with a few letters
// before it, and its end; …, where it leaves letters out. The whole path
// where nothing of it can be left out. That tells apart the few places a
// computer names alike; many that part in many places cannot all show
// apart in two lines, and a tile only opens the start sheet, which shows
// its whole directory before anything starts.
export function apart(paths: string[]): string[] {
  const letters = paths.map((p) => [...p])
  return paths.map((path, i) => {
    const parts = path.split('/')
    const others = paths.filter((_, j) => j !== i)
    let k = 1
    while (k < parts.length && others.some((o) => o.split('/').slice(-k).join('/') === parts.slice(-k).join('/'))) k++
    const whole = k >= parts.length || !parts.slice(0, -k).join('/')
    const end = whole ? [...path] : [...`/${parts.slice(-k).join('/')}`]
    if (columns(end) <= 24) return `${whole ? '' : '…'}${end.join('')}`
    // Where it parts from each other path, counted from its start: the
    // letter before the end they share, or its start, where it is the end
    // of the other.
    const parting = [
      ...new Set(letters.filter((_, j) => j !== i).map((other) => Math.max(0, end.length - sharedEnd(end, other) - 1))),
    ].sort((a, b) => a - b)
    // Each place with up to eight columns before it, the first, three the
    // others, back to a word, or to the start of a whole path; places close
    // together in one stretch, the last of which runs to the end.
    const stretches: [number, number][] = []
    for (const [n, at] of parting.entries()) {
      let from = at
      while (from > 0 && columns(end.slice(from - 1, at)) <= (n ? 3 : 8) && !divides(end[from - 1])) from--
      if (whole && from === 1) from = 0
      const last = stretches.at(-1)
      if (last && from <= last[1] + 3) last[1] = at + 1
      else stretches.push([from, at + 1])
    }
    const last = stretches.at(-1)
    if (last) last[1] = end.length
    return stretches.map(([from, to]) => `${whole && !from ? '' : '…'}${end.slice(from, to).join('')}`).join('')
  })
}

const divides = (c: string | undefined) => !!c && ['/', '-', '_', '.', ' '].includes(c)

// The wide letters of East Asian scripts, and emoji, which a monospaced
// face shows two columns wide.
const wideLetter =
  /[\u1100-\u115f\u2e80-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe30-\ufe4f\uff00-\uff60\uffe0-\uffe6]|\p{Extended_Pictographic}/u

// columns is how wide letters show in a monospaced face.
function columns(letters: string[]): number {
  let n = 0
  for (const c of letters) n += wideLetter.test(c) || (c.codePointAt(0) ?? 0) > 0x1ffff ? 2 : 1
  return n
}

// sharedEnd is how many letters two strings end alike in.
function sharedEnd(a: string[], b: string[]): number {
  let n = 0
  while (n < a.length && n < b.length && a[a.length - 1 - n] === b[b.length - 1 - n]) n++
  return n
}

// expand reads a path typed from ~ as one from the home directory.
export function expand(path: string, home?: string): string {
  if (!home || !(path === '~' || path.startsWith('~/'))) return path
  return `${home.replace(/\/$/, '')}${path.slice(1)}`
}

// snippet is the first words of what an agent wrote in Markdown, as plain
// text on one line, at most max characters.
export function snippet(text: string, max = 280): string {
  const words = text
    .split('\n')
    .filter((line) => !/^\s*(```|~~~)/.test(line))
    .map((line) =>
      line
        .replace(/^\s*(#{1,6}\s+|>\s?|[-*+]\s+(\[[ xX]\]\s+)?|\d+[.)]\s+)/, '')
        .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
        .replace(/(\*\*|__|`)/g, ''),
    )
    .join(' ')
    .replace(/\s+/g, ' ')
    .trim()
  return words.length > max ? `${words.slice(0, max - 1).trimEnd()}…` : words
}
