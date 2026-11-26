import { execFileSync } from 'node:child_process'
import { linkSync, readdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { hostname } from 'node:os'
import { join } from 'node:path'
import { FlowError } from './errors.ts'

// One process advances a run at a time. Its lock is a file lock.N, linked
// into place whole, naming the process and when it started. N counts up: a
// lock is taken as the next N after the highest, once that one is released
// or its process ended (or another process took its pid since), and
// whoever then sees a higher N than its own lost it. No lock is ever moved,
// and none but lower ones are removed, so the highest N never goes down
// and two processes never hold one run.

interface Holder {
  pid: number
  start: string
  host: string
}

// startOf is when a process started, as ps writes it in UTC and the C
// locale, so that runners in other time zones read the same.
function startOf(pid: number): string {
  try {
    return execFileSync('ps', ['-o', 'lstart=', '-p', String(pid)], {
      encoding: 'utf8',
      env: { ...process.env, TZ: 'UTC', LC_ALL: 'C' },
    }).trim()
  } catch {
    return ''
  }
}

function alive(h: Holder): boolean {
  if (h.host !== hostname()) return true // another machine's: never ours to judge
  try {
    process.kill(h.pid, 0)
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ESRCH') return false
  }
  const start = startOf(h.pid)
  return start === '' || h.start === '' || start === h.start
}

// read is what a lock holds: its holder, released, or nothing when it is
// gone.
function read(path: string): Holder | 'released' | undefined {
  try {
    const h = JSON.parse(readFileSync(path, 'utf8')) as Partial<Holder>
    return typeof h.pid === 'number' ? (h as Holder) : 'released'
  } catch {
    return undefined
  }
}

// generations are the numbers of dir's locks, highest last.
function generations(dir: string): number[] {
  return readdirSync(dir)
    .flatMap((f) => {
      const m = /^lock\.([1-9]\d*)$/.exec(f)
      return m ? [Number(m[1])] : []
    })
    .sort((a, b) => a - b)
}

// holder is the process that holds dir's lock, if one does.
export function holder(dir: string): Holder | undefined {
  const top = generations(dir).at(-1)
  if (top === undefined) return undefined
  const h = read(join(dir, `lock.${top}`))
  return typeof h === 'object' && alive(h) ? h : undefined
}

export interface Lock {
  release(): void
}

// acquire takes dir's lock, or says which process advances the run.
export function acquire(dir: string, what: string): Lock {
  const me: Holder = { pid: process.pid, start: startOf(process.pid), host: hostname() }
  const draft = join(dir, `.lock.${me.pid}.${Date.now()}`)
  writeFileSync(draft, JSON.stringify(me), { mode: 0o600 })
  try {
    for (let tries = 0; tries < 20; tries++) {
      const top = generations(dir).at(-1) ?? 0
      if (top) {
        const h = read(join(dir, `lock.${top}`))
        if (h === undefined) continue // one that lost it took it back
        if (h !== 'released' && alive(h)) {
          throw new FlowError(
            `${what} is advancing in process ${h.pid}${h.host === me.host ? '' : ` on ${h.host}`}`,
            'tmag-flow status shows where it is; run the command again once that process ends',
          )
        }
      }
      const mine = join(dir, `lock.${top + 1}`)
      try {
        linkSync(draft, mine)
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code === 'EEXIST') continue
        throw e
      }
      if ((generations(dir).at(-1) ?? 0) > top + 1) {
        unlinkSync(mine) // a process that read the locks later took a higher one
        continue
      }
      for (const n of generations(dir)) if (n <= top) unlink(join(dir, `lock.${n}`))
      let held = true
      return {
        release() {
          if (!held) return
          held = false
          const done = `${draft}.released`
          writeFileSync(done, '{}', { mode: 0o600 })
          renameSync(done, mine)
        },
      }
    }
  } finally {
    unlink(draft)
  }
  throw new FlowError(`${what}: its lock changed hands while tmag-flow took it`, 'run the command again')
}

function unlink(path: string): void {
  try {
    unlinkSync(path)
  } catch {
    // gone already
  }
}
