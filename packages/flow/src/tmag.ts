import { spawn } from 'node:child_process'
import { accessSync, constants } from 'node:fs'
import { homedir } from 'node:os'
import { delimiter, join } from 'node:path'
import { FlowError } from './errors.ts'

// The tmag CLI, run with --json: tmag-flow reaches agents through nothing
// else. Every call is a process of the runner's own, so tmag proves who runs
// it from the runner's ancestry: the agent whose tool runs tmag-flow, or the
// person.

// A document tmag printed (one per line of its stdout).
export type Doc = Record<string, unknown>

export interface Result {
  // tmag's exit code: 0 ok, 1 error, 2 usage, 3 refused, 4 timeout; -1 when
  // the process was stopped before it exited.
  code: number
  docs: Doc[]
  stdout: string
  stderr: string
  // The error document's code ("refused:busy", "timeout", "ended", ...).
  error: string | undefined
  message: string | undefined
}

export const exit = { ok: 0, error: 1, usage: 2, refused: 3, timeout: 4 } as const

// What tmag says of who runs it.
export interface Caller {
  pane?: { server: string; id: string }
  by?: { agent: string; session: string }
}

// locate finds the tmag binary the agents' hooks run, as tmag's own launcher
// does: the tmux plugin's directory first, then PATH. TMAG_FLOW_TMAG names
// one instead.
export function locate(env: NodeJS.ProcessEnv = process.env): string {
  if (env.TMAG_FLOW_TMAG) return env.TMAG_FLOW_TMAG
  const home = env.HOME ?? homedir()
  const roots = [
    env.TMUX_PLUGIN_MANAGER_PATH?.replace(/\/+$/, ''),
    join(home, '.tmux/plugins'),
    join(env.XDG_CONFIG_HOME ?? join(home, '.config'), 'tmux/plugins'),
  ]
  for (const root of roots) {
    if (!root?.startsWith('/')) continue
    const bin = join(root, 'tmux-agent-experience/bin/tmag')
    if (executable(bin)) return bin
  }
  for (const dir of (env.PATH ?? '').split(delimiter)) {
    if (!dir.startsWith('/')) continue
    const bin = join(dir, 'tmag')
    if (executable(bin)) return bin
  }
  throw new FlowError(
    'tmag is not installed',
    'install the tmux plugin: https://github.com/ChrAlpha/tmux-agent-experience#install',
  )
}

function executable(path: string): boolean {
  try {
    accessSync(path, constants.X_OK)
    return true
  } catch {
    return false
  }
}

const maxOutput = 16 << 20

export class Tmag {
  readonly bin: string
  #help: Promise<string> | undefined

  constructor(bin: string) {
    this.bin = bin
  }

  // run runs tmag with args and --json, giving it stdin, until it exits or
  // signal aborts it.
  run(
    args: readonly string[],
    options: { stdin?: string; signal?: AbortSignal; json?: boolean } = {},
  ): Promise<Result> {
    const argv = options.json === false ? [...args] : [...args, '--json']
    return new Promise((resolve, reject) => {
      const child = spawn(this.bin, argv, { stdio: ['pipe', 'pipe', 'pipe'], signal: options.signal })
      const out: Buffer[] = []
      const err: Buffer[] = []
      let size = 0
      const keep = (into: Buffer[]) => (b: Buffer) => {
        size += b.length
        if (size <= maxOutput) into.push(b)
      }
      child.stdout.on('data', keep(out))
      child.stderr.on('data', keep(err))
      child.on('error', (e: NodeJS.ErrnoException) => {
        if (e.name === 'AbortError') return // close follows
        reject(
          new FlowError(
            `running ${this.bin}: ${e.message}`,
            'install the tmux plugin tmux-agent-experience, or set TMAG_FLOW_TMAG to its bin/tmag',
          ),
        )
      })
      child.stdin.on('error', () => {}) // a tmag that exits early closes it
      child.stdin.end(options.stdin ?? '')
      child.on('close', (code) => {
        const stdout = Buffer.concat(out).toString('utf8')
        const docs: Doc[] = []
        const unread = () =>
          new FlowError(
            `tmag ${args[0]} printed ${size > maxOutput ? 'more than 16 MiB' : 'a line that is no JSON document'}`,
            'check that the tmag tmag-flow runs speaks tmag/v2: tmag-flow version and tmag version show both',
          )
        // A call cut short by signal tells nothing: its caller stops.
        if (options.json !== false && !options.signal?.aborted) {
          if (size > maxOutput) return reject(unread())
          for (const line of stdout.split('\n')) {
            if (!line.trim()) continue
            try {
              docs.push(JSON.parse(line) as Doc)
            } catch {
              return reject(unread())
            }
          }
        }
        const failure = docs.find((d) => typeof d.error === 'object' && d.error !== null)?.error as
          | { code?: string; message?: string }
          | undefined
        resolve({
          code: code ?? -1,
          docs,
          stdout,
          stderr: Buffer.concat(err).toString('utf8'),
          error: failure?.code,
          message: failure?.message,
        })
      })
    })
  }

  // help is tmag's usage text, read once.
  help(): Promise<string> {
    this.#help ??= this.run(['--help'], { json: false }).then((r) => r.stdout + r.stderr)
    return this.#help
  }

  // hasStartName says whether this tmag names the windows it starts.
  async hasStartName(): Promise<boolean> {
    const help = await this.help()
    return /tmag start\b[^\n]*--name NAME/.test(help)
  }
}

// short is a session id as people read it: its first 8 characters, as tmag
// lists it, or more where one of among shares them. Codex's ids begin with
// the time, so sessions started within a minute share 8.
export function short(id: string, among: Iterable<string> = []): string {
  let n = 8
  for (const other of among) {
    if (other === id) continue
    while (n < id.length && other.startsWith(id.slice(0, n))) n++
  }
  return id.slice(0, n)
}

// nextOf is the next: line a failed call of tmag printed, if any.
export function nextOf(r: Result): string | undefined {
  return r.stderr
    .split('\n')
    .find((l) => l.startsWith('next: '))
    ?.slice('next: '.length)
}

// describe says what a failed call printed, for an error of tmag-flow's own.
export function describe(r: Result): string {
  const lines = r.stderr.trim().split('\n').filter(Boolean)
  return lines.length ? lines.join('; ') : `exit ${r.code}`
}

// refusal is the reason of a refused call ("busy" for "refused:busy").
export function refusal(r: Result): string | undefined {
  return r.code === exit.refused && r.error?.startsWith('refused:') ? r.error.slice('refused:'.length) : undefined
}
