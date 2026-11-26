import { UsageError } from './errors.ts'

const unit: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000 }

// parseDuration reads a duration as tmag writes one: 90s, 8m, 1h30m.
export function parseDuration(text: string, what = 'a duration'): number {
  const parts = [...text.matchAll(/(\d+)(ms|s|m|h)/g)]
  if (parts.length === 0 || parts.map((p) => p[0]).join('') !== text) {
    throw new UsageError(
      `${what} is a number and a unit (ms, s, m, h), as 90s, 8m or 1h30m; not ${JSON.stringify(text)}`,
    )
  }
  const ms = parts.reduce((sum, p) => sum + Number(p[1]) * (unit[p[2] as string] as number), 0)
  if (ms <= 0) throw new UsageError(`${what} must be longer than 0`)
  return ms
}

// The longest a timer of Node's waits at once.
const longestTimer = 2 ** 31 - 1

// after runs fn once ms have passed, however long that is, unless cleared.
export function after(ms: number, fn: () => void): { clear(): void } {
  const at = Date.now() + ms
  let t: NodeJS.Timeout
  const arm = () => {
    const left = at - Date.now()
    t = left > longestTimer ? setTimeout(arm, longestTimer) : setTimeout(fn, Math.max(0, left))
  }
  arm()
  return { clear: () => clearTimeout(t) }
}

// formatDuration writes ms the way tmag reads it, rounded up to a whole unit
// tmag takes.
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.max(1, Math.ceil(ms))}ms`
  return `${Math.ceil(ms / 1000)}s`
}
