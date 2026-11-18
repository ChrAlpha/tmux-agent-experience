// The world the end-to-end tests run in: tmag and tmux-agent-experience's
// fake agents, built from the checkout TMAG_SRC names, in a private tmux
// server under a temporary TMUX_TMPDIR, with tmag serve in a window of it,
// and the built page served by `vite preview`, which proxies /v2/ to serve as
// tailscale serve does. Nothing touches the developer's tmux server or
// credentials.

import { execFileSync } from 'node:child_process'
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Credential, Session } from '../src/contract'

/** Where the page is served; tmag serve accepts only this address. */
export const origin = 'http://localhost:4173'

export interface Agent {
  name: string
  agent: 'claude' | 'codex' | 'pi'
  session: string
}

export const agents = {
  api: { name: 'api', agent: 'claude', session: '0a000000-0000-7000-8000-00000000000a' },
  web: { name: 'web', agent: 'codex', session: '0b000000-0000-7000-8000-00000000000b' },
  docs: { name: 'docs', agent: 'pi', session: '0c000000-0000-7000-8000-00000000000c' },
} satisfies Record<string, Agent>

const dir = () => {
  const d = process.env.TMAG_E2E_DIR
  if (!d) throw new Error('the world is not set up: run the tests through playwright test')
  return d
}

function env(): NodeJS.ProcessEnv {
  const d = dir()
  const { TMUX: _, TMUX_PANE: __, ...rest } = process.env
  return {
    ...rest,
    TMUX_TMPDIR: join(d, 'sock'),
    TMAG_STATE_DIR: join(d, 'state'),
    TMAG_AUTH_DIR: join(d, 'auth'),
    CODEX_HOME: join(d, 'codex'),
    PATH: `${join(d, 'bin')}:${process.env.PATH}`,
  }
}

export const tmux = (...args: string[]) =>
  execFileSync('tmux', ['-L', 'e2e', '-f', '/dev/null', ...args], { env: env(), encoding: 'utf8' })

// person runs a command as the person at the computer: in the tmux server,
// outside the process tree of whatever runs the tests, which may be an agent
// that tmag would refuse credential changes.
export function person(command: string): string {
  const out = join(dir(), `person-${Date.now()}.out`)
  tmux('run-shell', `${command} > ${out} 2>&1`)
  return readFileSync(out, 'utf8')
}

/** A pairing code, as `tmag auth pair` prints it. */
export const pairingCode = (): string => JSON.parse(person('tmag auth pair e2e --json')).pairing.code

/** A credential of its own, redeemed as the page would. */
export async function credential(): Promise<Credential> {
  const res = await fetch(`${origin}/v2/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: pairingCode() }),
  })
  if (!res.ok) throw new Error(`pairing failed: HTTP ${res.status} ${await res.text()}`)
  const { id, label, token } = (await res.json()).credential
  return { id, label, token }
}

/** What an agent was given and how its dialogs were answered, line by line. */
export const said = (a: Agent): string[] =>
  readFileSync(join(dir(), 'work', a.name, 'out'), 'utf8')
    .trimEnd()
    .split('\n')

export function session(a: Agent): Session | undefined {
  const list = JSON.parse(execFileSync('tmag', ['list', '--json'], { env: env(), encoding: 'utf8' }))
  return list.sessions.find((s: Session) => s.id === a.session)
}

/** Appends a turn to a Claude Code agent's transcript, as Claude Code writes one. */
export function addTurn(a: Agent, turn: { id: string; peer?: string; prompt?: string; reply: string }) {
  const lines = [
    turn.peer
      ? {
          type: 'user',
          promptId: turn.id,
          isSidechain: false,
          isMeta: true,
          origin: { kind: 'peer', name: 'codex 0b000000 via tmag', body: turn.peer },
          message: { role: 'user', content: 'x' },
        }
      : {
          type: 'user',
          promptId: turn.id,
          isSidechain: false,
          origin: { kind: 'human' },
          message: { role: 'user', content: turn.prompt ?? '' },
        },
    {
      type: 'assistant',
      isSidechain: false,
      message: { id: `m-${turn.id}`, role: 'assistant', content: [{ type: 'text', text: turn.reply }] },
    },
  ]
  const path = join(dir(), 'work', a.name, `${a.session}.jsonl`)
  writeFileSync(path, lines.map((l) => `${JSON.stringify(l)}\n`).join(''), { flag: 'a' })
}

export function startServe() {
  const port = process.env.TMAG_E2E_SERVE_PORT
  tmux(
    'new-window',
    '-d',
    '-n',
    'serve',
    `exec tmag serve --addr 127.0.0.1:${port} --host localhost:4173 --origin ${origin}`,
  )
}

export const stopServe = () => tmux('kill-window', '-t', 'main:serve')

async function until(what: string, ok: () => Promise<boolean> | boolean, ms = 15_000) {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 100))) {
    if (
      await Promise.resolve()
        .then(ok)
        .catch(() => false)
    )
      return
  }
  throw new Error(`timed out waiting for ${what}`)
}

const freePort = () =>
  new Promise<number>((resolve, reject) => {
    const s = createServer().listen(0, '127.0.0.1', () => {
      const a = s.address()
      s.close(() => (typeof a === 'object' && a ? resolve(a.port) : reject(new Error('no port'))))
    })
  })

// setup builds the world before the tests and returns its teardown.
export default async function setup() {
  const src = process.env.TMAG_SRC
  if (!src) throw new Error('TMAG_SRC must name a checkout of tmux-agent-experience to test against')
  const d = mkdtempSync(join(tmpdir(), 'tmag-client-e2e-'))
  process.env.TMAG_E2E_DIR = d
  process.env.TMAG_E2E_SERVE_PORT = String(await freePort())
  for (const sub of ['bin', 'sock', 'state', 'auth', 'codex']) mkdirSync(join(d, sub), { recursive: true })
  execFileSync('go', ['build', '-o', join(d, 'bin', 'tmag'), './cmd/tmag'], { cwd: src, stdio: 'inherit' })
  execFileSync('go', ['build', '-o', join(d, 'fake'), './internal/e2e/fakeagent'], { cwd: src, stdio: 'inherit' })
  for (const a of ['claude', 'codex', 'pi']) {
    copyFileSync(join(d, 'fake'), join(d, 'bin', a))
    chmodSync(join(d, 'bin', a), 0o755)
  }

  tmux('new-session', '-d', '-s', 'main', '-x', '160', '-y', '45', '/bin/sh')
  for (const a of Object.values(agents)) {
    const work = join(d, 'work', a.name)
    mkdirSync(work, { recursive: true })
    writeFileSync(join(work, 'out'), '')
    const fake = [
      `TMAG_BIN=${join(d, 'bin', 'tmag')}`,
      `FAKE_SESSION=${a.session}`,
      'FAKE_HOOKS=1',
      'FAKE_ANSWER=1',
      'FAKE_KEYS=1',
      'FAKE_TURN_MS=2500',
      `FAKE_OUT=${join(work, 'out')}`,
    ]
    if (a.agent === 'codex') fake.push(`FAKE_TITLE=${a.session.slice(0, 29)}... | ${a.name}`)
    tmux('new-window', '-d', '-n', a.name, '-c', work, ...fake.flatMap((e) => ['-e', e]), join(d, 'bin', a.agent))
  }
  startServe()
  await until('tmag serve', async () => !!(await fetch(`http://127.0.0.1:${process.env.TMAG_E2E_SERVE_PORT}/v2/info`)))
  await until('the agents', () => Object.values(agents).every((a) => session(a)?.state === 'idle'))

  process.env.TMAG_SERVE = `http://127.0.0.1:${process.env.TMAG_E2E_SERVE_PORT}`
  const { preview } = await import('vite')
  const server = await preview({ preview: { host: '127.0.0.1', port: 4173, strictPort: true } })

  return async () => {
    await server.close()
    try {
      tmux('kill-server')
    } catch {}
    rmSync(d, { recursive: true, force: true })
  }
}
