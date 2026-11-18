// A file's patch, as `changes --file PATH --json` gives it, read into lines a
// person can comment: each with the kind of line it is and its numbers in
// the file before (old) and after (new).

export interface Line {
  kind: 'meta' | 'hunk' | 'context' | 'added' | 'removed' | 'note'
  text: string
  /** Its line in the file as HEAD has it. */
  old?: number
  /** Its line in the file as the work tree has it. */
  new?: number
}

const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/

// lines reads a unified diff. What precedes its first hunk is meta: the
// headers git writes; a line no hunk explains stays meta too.
export function lines(patch: string): Line[] {
  const out: Line[] = []
  let old = 0
  let now = 0
  let inHunk = false
  for (const text of patch.replace(/\n$/, '').split('\n')) {
    const h = hunk.exec(text)
    if (h) {
      old = Number(h[1])
      now = Number(h[2])
      inHunk = true
      out.push({ kind: 'hunk', text })
    } else if (!inHunk || text.startsWith('diff --git ')) {
      inHunk = false
      out.push({ kind: 'meta', text })
    } else if (text.startsWith('+')) out.push({ kind: 'added', text: text.slice(1), new: now++ })
    else if (text.startsWith('-')) out.push({ kind: 'removed', text: text.slice(1), old: old++ })
    else if (text.startsWith(' ')) out.push({ kind: 'context', text: text.slice(1), old: old++, new: now++ })
    else if (text.startsWith('\\')) out.push({ kind: 'note', text })
    else out.push({ kind: 'meta', text })
  }
  return out
}

/**
 * A line of a file a comment is about, as it was when the comment was
 * begun: its number, its side, its code, and digests of it and of the lines
 * of its hunk around it, by which it is found again as the patch changes.
 */
export interface Anchor {
  /** Its number after, or before for a line removed. */
  line: number
  removed: boolean
  /** Its text, as much as a comment quotes. */
  code: string
  /** A digest of all its text. */
  hash: number
  /** Digests of the lines of its hunk just before it, and just after. */
  before: number[]
  after: number[]
}

/** What the person says of a line of a file. */
export interface Comment extends Anchor {
  file: string
  text: string
}

/** Whether a line can be commented: a line of a hunk. */
export const commentable = (l: Line) => l.kind === 'added' || l.kind === 'removed' || l.kind === 'context'

// digest is FNV-1a over a line's text: a comment keeps no more of the lines
// around its own than this.
function digest(text: string): number {
  let h = 0x811c9dc5
  for (let i = 0; i < text.length; i++) h = Math.imul(h ^ text.charCodeAt(i), 0x01000193)
  return h >>> 0
}

// around is the digests of up to three lines of the hunk next to row i, in
// direction step.
function around(rows: readonly Line[], i: number, step: 1 | -1): number[] {
  const out: number[] = []
  for (let j = i + step; out.length < 3; j += step) {
    const l = rows[j]
    if (!l || !commentable(l)) break
    out.push(digest(l.text))
  }
  return out
}

/** The anchor of row i of a patch, if it can be commented. */
export function anchor(rows: readonly Line[], i: number): Anchor | undefined {
  const l = rows[i]
  const line = l && (l.new ?? l.old)
  if (!l || !commentable(l) || line === undefined) return undefined
  return {
    line,
    removed: l.kind === 'removed',
    code: l.text.slice(0, 500),
    hash: digest(l.text),
    before: around(rows, i, -1),
    after: around(rows, i, 1),
  }
}

// shared counts the lines two runs of digests have in common, each line of
// either counted once, so that one blank line matches one blank line.
function shared(a: readonly number[], b: readonly number[]): number {
  const left = [...b]
  let n = 0
  for (const h of a) {
    const i = left.indexOf(h)
    if (i < 0) continue
    left.splice(i, 1)
    n++
  }
  return n
}

// where finds the row an anchor is on in a patch, as the patch changes under
// it: of the lines on its side with its text, the one whose lines around it
// are most like those it had, and at least half of them; -1 when none is,
// or when two are alike, as when the line was removed and another like it
// remains, or another like it came near.
export function where(rows: readonly Line[], a: Anchor): number {
  const had = a.before.length + a.after.length
  let at = -1
  let best = -1
  let tied = false
  rows.forEach((_, i) => {
    const b = anchor(rows, i)
    if (!b || b.removed !== a.removed || b.hash !== a.hash) return
    const alike = shared(a.before, b.before) + shared(a.after, b.after)
    if (alike > best) [at, best, tied] = [i, alike, false]
    else if (alike === best) tied = true
  })
  return at < 0 || tied || best * 2 < had ? -1 : at
}

const digests = (x: unknown) => Array.isArray(x) && x.length <= 3 && x.every((h) => Number.isInteger(h))

/** Whether what the tab kept is a comment. */
export const isComment = (x: unknown): x is Comment => {
  const c = x as Comment
  return (
    typeof c === 'object' &&
    c !== null &&
    typeof c.file === 'string' &&
    Number.isInteger(c.line) &&
    typeof c.removed === 'boolean' &&
    typeof c.code === 'string' &&
    Number.isInteger(c.hash) &&
    digests(c.before) &&
    digests(c.after) &&
    typeof c.text === 'string'
  )
}

// prompt writes comments as a prompt an agent reads: each the file and line
// it is about, numbered as its file's patch has the line now where it can be
// found there, then what was said, then the line quoted.
export function prompt(comments: readonly Comment[], rowsOf: (file: string) => readonly Line[]): string {
  return comments
    .map((c) => {
      const rows = rowsOf(c.file)
      const now = anchor(rows, where(rows, c))
      return `${c.file}:${now?.line ?? c.line}${c.removed ? ' (removed)' : ''}: ${c.text.trim()}\n> ${c.code}`
    })
    .join('\n')
}
