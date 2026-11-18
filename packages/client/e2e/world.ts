// The world the end-to-end tests run in: tmag and tmux-agent-experience's
// fake agents, built from the checkout TMAG_SRC names, in a private tmux
// server under a temporary TMUX_TMPDIR, with tmag serve in a window of it,
// and the built page served by `vite preview`, which proxies /v2/ to serve as
// tailscale serve does. A second computer is a tmux server of its own, with
// tmag's state and credentials of its own, its own tmag serve and an agent,
// which the page reaches at /two/; /again/ reaches the first computer at a
// second address. Nothing touches the developer's tmux server or
// credentials.

import { execFileSync } from 'node:child_process'
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Credential, Session } from '../src/contract'

/** Where the page is served; tmag serve accepts only this address. */
export const origin = 'http://localhost:4173'

/** A computer, by the name the page reaches it at: '' for the first. */
export type At = '' | 'two'

export interface Agent {
  name: string
  agent: 'claude' | 'codex' | 'pi'
  session: string
  at: At
}

export const agents = {
  api: { name: 'api', agent: 'claude', session: '0a000000-0000-7000-8000-00000000000a', at: '' },
  web: { name: 'web', agent: 'codex', session: '0b000000-0000-7000-8000-00000000000b', at: '' },
  docs: { name: 'docs', agent: 'pi', session: '0c000000-0000-7000-8000-00000000000c', at: '' },
  ops: { name: 'ops', agent: 'claude', session: '0d000000-0000-7000-8000-00000000000d', at: 'two' },
} satisfies Record<string, Agent>

const dir = () => {
  const d = process.env.TMAG_E2E_DIR
  if (!d) throw new Error('the world is not set up: run the tests through playwright test')
  return d
}

// What is a computer's own: its tmux servers' directory, tmag's state and
// its credentials.
const own = (at: At) => ({
  TMUX_TMPDIR: join(dir(), at ? `sock-${at}` : 'sock'),
  TMAG_STATE_DIR: join(dir(), at ? `state-${at}` : 'state'),
  TMAG_AUTH_DIR: join(dir(), at ? `auth-${at}` : 'auth'),
})

function env(at: At = ''): NodeJS.ProcessEnv {
  const d = dir()
  const { TMUX: _, TMUX_PANE: __, ...rest } = process.env
  return {
    ...rest,
    ...own(at),
    CODEX_HOME: join(d, 'codex'),
    PATH: `${join(d, 'bin')}:${process.env.PATH}`,
  }
}

// tmuxOn runs tmux on a computer's server, which starts with that
// computer's environment, and gives it to what runs there.
export const tmuxOn = (at: At, ...args: string[]) =>
  execFileSync('tmux', ['-L', at ? `e2e-${at}` : 'e2e', '-f', '/dev/null', ...args], { env: env(at), encoding: 'utf8' })

/** tmux on the first computer's server. */
export const tmux = (...args: string[]) => tmuxOn('', ...args)

// person runs a command as the person at a computer: in the tmux server,
// outside the process tree of whatever runs the tests, which may be an agent
// that tmag would refuse credential changes.
export function person(command: string, at: At = ''): string {
  const out = join(dir(), `person-${Date.now()}.out`)
  tmuxOn(at, 'run-shell', `${command} > ${out} 2>&1`)
  return readFileSync(out, 'utf8')
}

/** A pairing code of a computer, as `tmag auth pair` prints it. */
export const pairingCode = (at: At = ''): string => JSON.parse(person('tmag auth pair e2e --json', at)).pairing.code

/** A credential of its own on a computer, redeemed as the page would. */
export async function credential(at: At = ''): Promise<Credential> {
  const res = await fetch(`${origin}${at ? `/${at}` : ''}/v2/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: pairingCode(at) }),
  })
  if (!res.ok) throw new Error(`pairing failed: HTTP ${res.status} ${await res.text()}`)
  const { id, label, token } = (await res.json()).credential
  return { id, label, token }
}

/** Whether the first computer's tmag serves an operation. */
export const serves = (operation: string) => (process.env.TMAG_E2E_OPERATIONS ?? '').split(',').includes(operation)

/** What an agent was given and how its dialogs were answered, line by line. */
export const said = (a: Agent): string[] =>
  readFileSync(join(dir(), 'work', a.name, 'out'), 'utf8')
    .trimEnd()
    .split('\n')

export function session(a: Agent): Session | undefined {
  const list = JSON.parse(execFileSync('tmag', ['list', '--json'], { env: env(a.at), encoding: 'utf8' }))
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

const port = (at: At) => process.env[at ? `TMAG_E2E_SERVE_PORT_${at.toUpperCase()}` : 'TMAG_E2E_SERVE_PORT']

export function startServe(at: At = '') {
  tmuxOn(
    at,
    'new-window',
    '-d',
    '-n',
    'serve',
    `exec tmag serve --addr 127.0.0.1:${port(at)} --host localhost:4173 --origin ${origin}`,
  )
}

export const stopServe = (at: At = '') => tmuxOn(at, 'kill-window', '-t', 'main:serve')

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
  process.env.TMAG_E2E_SERVE_PORT_TWO = String(await freePort())
  for (const sub of ['bin', 'sock', 'state', 'auth', 'sock-two', 'state-two', 'auth-two', 'codex'])
    mkdirSync(join(d, sub), { recursive: true })
  execFileSync('go', ['build', '-o', join(d, 'bin', 'tmag'), './cmd/tmag'], { cwd: src, stdio: 'inherit' })
  execFileSync('go', ['build', '-o', join(d, 'fake'), './internal/e2e/fakeagent'], { cwd: src, stdio: 'inherit' })
  for (const a of ['claude', 'codex', 'pi']) {
    copyFileSync(join(d, 'fake'), join(d, 'bin', a))
    chmodSync(join(d, 'bin', a), 0o755)
  }

  for (const at of ['', 'two'] as const)
    tmuxOn(at, 'new-session', '-d', '-s', 'main', '-x', '160', '-y', '45', '/bin/sh')
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
    tmuxOn(
      a.at,
      'new-window',
      '-d',
      '-n',
      a.name,
      '-c',
      work,
      ...fake.flatMap((e) => ['-e', e]),
      join(d, 'bin', a.agent),
    )
  }
  for (const at of ['', 'two'] as const) {
    startServe(at)
    await until('tmag serve', async () => !!(await fetch(`http://127.0.0.1:${port(at)}/v2/info`)))
  }
  await until('the agents', () => Object.values(agents).every((a) => session(a)?.state === 'idle'))

  process.env.TMAG_SERVE = `http://127.0.0.1:${port('')}`
  process.env.TMAG_COMPUTERS = `two=http://127.0.0.1:${port('two')},again=${process.env.TMAG_SERVE}`
  const { preview } = await import('vite')
  const server = await preview({ preview: { host: '127.0.0.1', port: 4173, strictPort: true } })

  // What the first computer's tmag serves, which tests of what an older one
  // lacks ask for.
  const { token } = await credential()
  const info = await (await fetch(`${origin}/v2/info`, { headers: { Authorization: `Bearer ${token}` } })).json()
  process.env.TMAG_E2E_OPERATIONS = (info.operations ?? []).join(',')

  return async () => {
    await server.close()
    for (const at of ['', 'two'] as const) {
      try {
        tmuxOn(at, 'kill-server')
      } catch {}
    }
    rmSync(d, { recursive: true, force: true })
  }
}
