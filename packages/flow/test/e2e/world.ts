// A world for end-to-end tests: a private tmux server under a temporary
// TMUX_TMPDIR, tmag and its fake agent built from this repository or the
// checkout TMAG_SRC names, and a git repository to run plans in. Nothing
// reaches the developer's own tmux server, agents or tmag state.

import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

export const packageRoot = resolve(fileURLToPath(new URL('../..', import.meta.url)))
const src = process.env.TMAG_SRC || resolve(packageRoot, '../..')

// skip says why the end-to-end tests cannot run here, if they cannot.
export function skip(): string | false {
  for (const tool of ['go', 'tmux', 'git']) {
    if (spawnSync('sh', ['-c', `command -v ${tool}`]).status !== 0) return `${tool} is not installed`
  }
  return false
}

let built: string | undefined

// build builds tmag and the fake agents once per test process, in a
// directory that goes when the process exits.
function build(): string {
  if (built) return built
  const bin = mkdtempSync(join(tmpdir(), 'tmag-flow-bin-'))
  process.once('exit', () => rmSync(bin, { recursive: true, force: true }))
  const go = (pkg: string, out: string) =>
    execFileSync('go', ['build', '-o', join(bin, out), pkg], { cwd: src, stdio: ['ignore', 'ignore', 'inherit'] })
  go('./cmd/tmag', 'tmag')
  go('./internal/e2e/fakeagent', 'fake')
  // The agents run as their own names, so tmag proves them as agents, each
  // with a session id of its own: a template's command is one of these.
  const agents = join(bin, 'agents')
  mkdirSync(agents)
  for (const agent of ['claude', 'codex', 'pi', 'omp']) {
    execFileSync('ln', [join(bin, 'fake'), join(bin, agent)])
    writeFileSync(
      join(agents, agent),
      `#!/bin/sh
h=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \\n')
FAKE_SESSION=$(printf '%s' "$h" | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12})$/\\1-\\2-\\3-\\4-\\5/')
export FAKE_SESSION
case \${FAKE_SLOW:-} in *" $0 "*|*" \${0##*/} "*) export FAKE_TURN_MS=60000;; esac
exec ${join(bin, agent)} "$@"
`,
      { mode: 0o755 },
    )
  }
  // TMAG_FLOW_CLI runs another tmag-flow, as the package installs it.
  const flow = join(bin, 'tmag-flow')
  const cli = process.env.TMAG_FLOW_CLI ?? `node ${join(packageRoot, 'src/cli.ts')}`
  writeFileSync(flow, `#!/bin/sh\nexec ${cli} "$@"\n`, { mode: 0o755 })
  built = bin
  return bin
}

let worlds = 0

export class World {
  readonly bin: string
  readonly dir: string
  readonly sock: string
  readonly repo: string
  readonly name: string
  readonly env: NodeJS.ProcessEnv

  constructor() {
    this.bin = build()
    this.dir = mkdtempSync(join(tmpdir(), 'tmag-flow-e2e-'))
    this.sock = mkdtempSync('/tmp/tfe-') // short: a socket's path is bounded
    this.repo = join(this.dir, 'repo')
    this.name = `e2e${process.pid}-${++worlds}`
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      PATH: `${join(this.bin, 'agents')}:${this.bin}:${process.env.PATH}`,
      TMUX_TMPDIR: this.sock,
      TMAG_STATE_DIR: join(this.sock, 'state'),
      TMAG_AUTH_DIR: join(this.dir, 'auth'),
      TMAG_FLOW_TMAG: join(this.bin, 'tmag'),
      SHELL: '/bin/sh',
    }
    delete env.TMUX
    delete env.TMUX_PANE
    this.env = env
    mkdirSync(this.repo)
    this.git('init', '-q', '-b', 'main')
    this.git('-c', 'user.name=t', '-c', 'user.email=t@example.com', 'commit', '-q', '--allow-empty', '-m', 'init')
    this.tmux('-f', '/dev/null', 'new-session', '-d', '-s', 'main', '-x', '200', '-y', '50', '/bin/sh')
    for (const [k, v] of Object.entries({
      TMAG_BIN: join(this.bin, 'tmag'),
      TMAG_STATE_DIR: env.TMAG_STATE_DIR as string,
      TMAG_FLOW_TMAG: env.TMAG_FLOW_TMAG as string,
      FAKE_HOOKS: '1',
      FAKE_INBOX: '1',
      FAKE_TURN_MS: '300',
    })) {
      this.tmux('set-environment', '-g', k, v)
    }
    for (const t of ['impl', 'sol', 'astra']) {
      this.tmux('set-option', '-g', `@tmag-start-${t}`, join(this.bin, 'agents', t === 'impl' ? 'pi' : 'claude'))
    }
  }

  git(...args: string[]): string {
    return execFileSync('git', ['-C', this.repo, ...args], { encoding: 'utf8' }).trim()
  }

  tmux(...args: string[]): string {
    return execFileSync('tmux', ['-L', this.name, ...args], { env: this.env, encoding: 'utf8' }).trim()
  }

  tmag(...args: string[]): { code: number; out: string; docs: Record<string, unknown>[] } {
    const r = spawnSync(join(this.bin, 'tmag'), [...args, '--json'], { env: this.env, encoding: 'utf8' })
    const docs = r.stdout
      .split('\n')
      .filter(Boolean)
      .flatMap((l) => {
        try {
          return [JSON.parse(l) as Record<string, unknown>]
        } catch {
          return []
        }
      })
    return { code: r.status ?? -1, out: r.stdout + r.stderr, docs }
  }

  // person runs script in a window of the private server, as a person at a
  // shell would, and returns once it exits: its output and exit status.
  person(script: string, timeout = 60_000): Promise<{ code: number; out: string }> {
    const id = `p${Date.now()}${Math.random().toString(36).slice(2, 6)}`
    const out = join(this.dir, `${id}.out`)
    const code = join(this.dir, `${id}.code`)
    writeFileSync(join(this.dir, `${id}.sh`), `cd ${this.repo}\n${script}\n`)
    this.tmux(
      'new-window',
      '-d',
      '-n',
      id,
      '-c',
      this.repo,
      `/bin/sh -c 'sh ${join(this.dir, `${id}.sh`)} >${out} 2>&1; echo $? >${code}.tmp; mv ${code}.tmp ${code}; exec sleep 3600'`,
    )
    return waitFor(() => existsSync(code), timeout, `${script} did not finish`).then(() => ({
      code: Number(readFileSync(code, 'utf8').trim()),
      out: readFileSync(out, 'utf8'),
    }))
  }

  // background starts script as the person and returns at once.
  background(script: string): { out: string; done: () => boolean; code: () => number } {
    const id = `b${Date.now()}${Math.random().toString(36).slice(2, 6)}`
    const out = join(this.dir, `${id}.out`)
    const code = join(this.dir, `${id}.code`)
    writeFileSync(join(this.dir, `${id}.sh`), `cd ${this.repo}\n${script}\n`)
    this.tmux(
      'new-window',
      '-d',
      '-n',
      id,
      '-c',
      this.repo,
      `/bin/sh -c 'sh ${join(this.dir, `${id}.sh`)} >${out} 2>&1; echo $? >${code}.tmp; mv ${code}.tmp ${code}; exec sleep 3600'`,
    )
    return {
      out,
      done: () => existsSync(code),
      code: () => Number(readFileSync(code, 'utf8').trim()),
    }
  }

  journal(run: string): Record<string, unknown>[] {
    const path = join(this.repo, '.tmag-flow', run, 'journal.jsonl')
    if (!existsSync(path)) return []
    return readFileSync(path, 'utf8')
      .split('\n')
      .filter(Boolean)
      .map((l) => JSON.parse(l) as Record<string, unknown>)
  }

  events(session: string): Record<string, unknown>[] {
    return (this.tmag('inspect', session, '--events', '64').docs[0]?.events ?? []) as Record<string, unknown>[]
  }

  // paneOf is the pane that shows a session.
  paneOf(session: string): string {
    const s = this.tmag('inspect', session).docs[0]?.session as { panes?: { id: string }[] } | undefined
    const pane = s?.panes?.[0]?.id
    if (!pane) throw new Error(`no pane shows ${session}`)
    return pane
  }

  close(): void {
    try {
      this.tmux('kill-server')
    } catch {
      // gone already
    }
    rmSync(this.sock, { recursive: true, force: true })
    rmSync(this.dir, { recursive: true, force: true })
  }
}

export async function waitFor(ok: () => boolean, timeout: number, what: string): Promise<void> {
  const end = Date.now() + timeout
  while (!ok()) {
    if (Date.now() > end) throw new Error(`timed out: ${what}`)
    await new Promise((r) => setTimeout(r, 50))
  }
}

export function plan(name: string): string {
  return join(packageRoot, 'test', 'plans', `${name}.ts`)
}
