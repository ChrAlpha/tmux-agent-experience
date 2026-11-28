import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { afterEach, describe, it } from 'node:test'
import { plan, skip, World, waitFor } from './world.ts'

const why = skip()

describe('tmag-flow over tmag', { skip: why }, () => {
  let w: World
  afterEach(() => w?.close())

  const sends = (session: string) => w.events(session).filter((e) => e.event === 'Send').length
  const records = (run: string, t: string) => w.journal(run).filter((r) => r.t === t)
  const revisionAt = (ref: string) => new RegExp(`^${w.git('rev-parse', ref)}(?: \\(dirty\\))?$`)
  const bound = (run: string, role: string) =>
    records(run, 'role.bound')
      .filter((r) => r.role === role)
      .map((r) => r.session as string)
  const stateOf = (session: string) =>
    (w.tmag('inspect', session).docs[0]?.session as { state?: string } | undefined)?.state
  const runner = (run: string) => (records(run, 'invocation').at(-1)?.pid as number) ?? 0
  const alive = (pid: number) => {
    try {
      process.kill(pid, 0)
      return true
    } catch {
      return false
    }
  }
  // end types /end into the pane of a slow fake agent, ending its turn.
  const end = (session: string) => w.tmux('send-keys', '-t', w.paneOf(session), '/end', 'Enter')
  const killRunner = async (run: string) => {
    const pid = runner(run)
    const pgid = execFileSync('ps', ['-o', 'pgid=', '-p', String(pid)], { encoding: 'utf8' }).trim()
    process.kill(-Number(pgid), 'SIGKILL')
    await waitFor(() => !alive(pid), 10_000, 'the runner to die')
  }

  it('halts for a decision, and a resume replays what was done', async () => {
    w = new World()
    const first = await w.person(`tmag-flow start ${plan('basic')} --run b1 --json`)
    assert.equal(first.code, 5, first.out)
    assert.match(first.out, /next: tmag-flow decide b1 gate-1-failed retry\|stop, then tmag-flow resume b1/)
    const [impl] = bound('b1', 'impl')
    assert.ok(impl)
    assert.equal(sends(impl), 1)

    const status = await w.person('tmag-flow status b1 --json')
    const v = JSON.parse(status.out) as {
      state: string
      halts: { key: string }[]
      roles: { name: string; state: string }[]
    }
    assert.equal(v.state, 'halted')
    assert.deepEqual(
      v.halts.map((h) => h.key),
      ['gate-1-failed'],
    )
    assert.equal(v.roles[0]?.state, 'idle')

    assert.equal((await w.person('tmag-flow decide b1 gate-1-failed maybe')).code, 2)
    assert.equal((await w.person('tmag-flow decide b1 gate-1-failed stop')).code, 0)
    const done = await w.person('tmag-flow resume b1 --json')
    assert.equal(done.code, 0, done.out)
    const doc = JSON.parse(done.out.trim().split('\n').at(-1) as string) as {
      state: string
      result: { choice: string; first: string }
    }
    assert.equal(doc.state, 'done')
    assert.equal(doc.result.choice, 'stop')
    assert.equal(doc.result.first, 'answered: unit 1 done')
    assert.equal(sends(impl), 1, 'a resume replays the ask from the journal')
    assert.equal((await w.person('tmag-flow resume b1')).code, 0)
  })

  it('picks up an ask whose runner died while the turn ran', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    w.background(`tmag-flow start ${plan('one')} --run c1`)
    await waitFor(() => records('c1', 'delivered').length === 1, 30_000, 'the ask to be delivered')
    await killRunner('c1')
    const [impl] = bound('c1', 'impl')
    end(impl as string)
    const done = await w.person('tmag-flow resume c1')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'answered: one done')
    assert.equal(sends(impl as string), 1, 'the prompt was given once')
  })

  it('stops when its time is spent, and resumes', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const first = await w.person(`tmag-flow start ${plan('one')} --run t1 --for 3s --json`)
    assert.equal(first.code, 4, first.out)
    assert.match(first.out, /next: tmag-flow resume t1/)
    const [impl] = bound('t1', 'impl')
    end(impl as string)
    const done = await w.person('tmag-flow resume t1')
    assert.equal(done.code, 0, done.out)
    assert.equal(sends(impl as string), 1)
  })

  it('refuses a second process while one advances the run', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const bg = w.background(`tmag-flow start ${plan('one')} --run l1`)
    await waitFor(() => records('l1', 'delivered').length === 1, 30_000, 'the ask to be delivered')
    const second = await w.person('tmag-flow resume l1')
    assert.equal(second.code, 1, second.out)
    assert.match(second.out, /is advancing in process \d+/)
    const decide = await w.person('tmag-flow decide l1 impl-1 rerun')
    assert.equal(decide.code, 1, decide.out)
    end(bound('l1', 'impl')[0] as string)
    await waitFor(bg.done, 30_000, 'the run to finish')
    assert.equal(bg.code(), 0)
  })

  it('starts another session for a role whose session ended', async () => {
    w = new World()
    const go = join(w.dir, 'go')
    const bg = w.background(`tmag-flow start ${plan('twice')} --run r1 --arg go=${go}`)
    await waitFor(() => records('r1', 'intent').some((r) => r.key === 'go'), 30_000, 'the gate to start')
    const [first] = bound('r1', 'impl')
    w.tmux('kill-window', '-t', w.paneOf(first as string))
    writeFileSync(go, '')
    await waitFor(bg.done, 30_000, 'the run to finish')
    assert.equal(bg.code(), 0)
    const sessions = bound('r1', 'impl')
    assert.equal(sessions.length, 2)
    assert.notEqual(sessions[0], sessions[1])
    assert.equal(records('r1', 'role.gone').length, 1)
  })

  it('asks roles at once, within its concurrency', async () => {
    w = new World()
    const r = await w.person(`tmag-flow start ${plan('pair')} --run p1 --concurrency 1 --json`)
    assert.equal(r.code, 0, r.out)
    const doc = JSON.parse(r.out.trim().split('\n').at(-1) as string) as { result: Record<string, string> }
    assert.equal(doc.result.sol, 'answered: sol verdict')
    assert.equal(doc.result.astra, 'answered: astra verdict')
    assert.match(doc.result.report as string, /\.tmag-flow\/p1\/artifacts\/reports\/r1\.md$/)
  })

  it('halts on a step the plan now describes differently, and reruns it once decided', async () => {
    w = new World()
    const prompt = join(w.dir, 'prompt')
    writeFileSync(prompt, 'Do it.\nfirst prompt')
    assert.equal((await w.person(`tmag-flow start ${plan('diverge')} --run d1 --arg prompt=${prompt}`)).code, 5)
    writeFileSync(prompt, 'Do it.\nsecond prompt')
    assert.equal((await w.person('tmag-flow decide d1 go-on go')).code, 0)
    const halted = await w.person('tmag-flow resume d1')
    assert.equal(halted.code, 5, halted.out)
    assert.match(halted.out, /impl-1 halted: the plan now describes this ask differently/)
    assert.equal((await w.person('tmag-flow decide d1 impl-1 rerun')).code, 0)
    const done = await w.person('tmag-flow resume d1 --json')
    assert.equal(done.code, 0, done.out)
    const doc = JSON.parse(done.out.trim().split('\n').at(-1) as string) as { result: { text: string; choice: string } }
    assert.deepEqual(doc.result, { text: 'answered: second prompt', choice: 'go' })
  })

  it('halts on an ask the plan changed while its turn ran', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const prompt = join(w.dir, 'prompt')
    writeFileSync(prompt, 'Do it.\nold prompt')
    w.background(`tmag-flow start ${plan('diverge')} --run f1 --arg prompt=${prompt}`)
    await waitFor(() => records('f1', 'delivered').length === 1, 30_000, 'the ask to be delivered')
    await killRunner('f1')
    writeFileSync(prompt, 'Do it.\nnew prompt')
    end(bound('f1', 'impl')[0] as string)
    const halted = await w.person('tmag-flow resume f1')
    assert.equal(halted.code, 5, halted.out)
    assert.match(halted.out, /impl-1 halted: the plan now describes this ask differently from the one that started/)
    assert.equal(records('f1', 'done').length, 0, 'the old prompt answers no step')
  })

  it('halts an ask whose session ended after it was given the prompt, and asks another once decided', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const bg = w.background(`tmag-flow start ${plan('one')} --run e1`)
    await waitFor(() => records('e1', 'delivered').length === 1, 30_000, 'the ask to be delivered')
    const [first] = bound('e1', 'impl')
    w.tmux('kill-window', '-t', w.paneOf(first as string))
    await waitFor(bg.done, 30_000, 'the run to halt')
    assert.equal(bg.code(), 5)
    assert.equal(bound('e1', 'impl').length, 1, 'no other session was given the prompt')
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' ')
    assert.equal((await w.person('tmag-flow decide e1 impl-1 rerun')).code, 0)
    const done = await w.person('tmag-flow resume e1')
    assert.equal(done.code, 0, done.out)
    const [, second] = bound('e1', 'impl')
    assert.ok(second && second !== first)
    assert.equal(sends(second), 1)
  })

  // failing is a tmag whose send exits 1 having delivered nothing, as a
  // channel that fails does.
  const failing = () => {
    const shim = join(w.dir, 'failing-tmag')
    writeFileSync(
      shim,
      `#!/bin/sh\nif [ "$1" = send ]; then echo 'tmag send: the channel failed' >&2; exit 1; fi\nexec ${join(w.bin, 'tmag')} "$@"\n`,
      { mode: 0o755 },
    )
    return shim
  }
  const planFile = (name: string, body: string) => {
    const path = join(w.dir, `${name}.mjs`)
    writeFileSync(path, body)
    return path
  }

  it('takes no turn of an earlier attempt for the attempt a decision reran', async () => {
    w = new World()
    const prompt = join(w.dir, 'prompt')
    writeFileSync(prompt, 'Do it.\nold prompt')
    assert.equal((await w.person(`tmag-flow start ${plan('diverge')} --run a1 --arg prompt=${prompt}`)).code, 5)
    writeFileSync(prompt, 'Do it.\nnew prompt')
    assert.equal((await w.person('tmag-flow decide a1 go-on go')).code, 0)
    assert.equal((await w.person('tmag-flow resume a1')).code, 5)
    assert.equal((await w.person('tmag-flow decide a1 impl-1 rerun')).code, 0)
    const cut = await w.person(`TMAG_FLOW_TMAG=${failing()} tmag-flow resume a1`)
    assert.equal(cut.code, 1, cut.out)
    assert.match(cut.out, /impl-1: tmag could not give impl the prompt: tmag send: the channel failed/)
    const done = await w.person('tmag-flow resume a1 --json')
    assert.equal(done.code, 0, done.out)
    const doc = JSON.parse(done.out.trim().split('\n').at(-1) as string) as { result: { text: string } }
    assert.equal(doc.result.text, 'answered: new prompt')
  })

  it('takes no turn whose prompt only quotes the line of its own', async () => {
    w = new World()
    const cut = await w.person(`TMAG_FLOW_TMAG=${failing()} tmag-flow start ${plan('one')} --run q1`)
    assert.equal(cut.code, 1, cut.out)
    const [impl] = bound('q1', 'impl')
    const quoted = await w.person(
      `printf '%s\\n' 'What is this line?' 'tmag-flow run q1, step impl-1, attempt 1' 'other work' | tmag send ${impl} --wait --timeout 20s -`,
    )
    assert.equal(quoted.code, 0, quoted.out)
    const done = await w.person('tmag-flow resume q1')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'answered: one done')
  })

  it("counts a turn's timeout from its prompt, however many invocations it takes", async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const bg = w.background(`tmag-flow start ${plan('one')} --run b2 --ask-timeout 4s`)
    await waitFor(() => records('b2', 'delivered').length === 1, 30_000, 'the prompt to be delivered')
    process.kill(runner('b2'), 'SIGINT')
    await waitFor(bg.done, 30_000, 'the runner to stop')
    assert.equal(bg.code(), 4, readFileSync(bg.out, 'utf8'))
    const at = Date.parse(records('b2', 'delivered')[0]?.at as string)
    await waitFor(() => Date.now() > at + 4500, 10_000, 'the turn to outlive its timeout')
    // Less time than a fresh timeout: only the first one, kept, halts it.
    const halted = await w.person('tmag-flow resume b2 --for 2s')
    assert.equal(halted.code, 5, halted.out)
    assert.match(halted.out, /impl-1 halted: impl \(\w+\) has not finished its turn within 4s/)
    const [impl] = bound('b2', 'impl')
    assert.equal((await w.person('tmag-flow decide b2 impl-1 wait')).code, 0)
    end(impl as string)
    const done = await w.person('tmag-flow resume b2')
    assert.equal(done.code, 0, done.out)
    assert.equal(sends(impl as string), 1)
  })

  it('asks again a decision whose question the plan changed', async () => {
    w = new World()
    const p = planFile('approve', `export default (flow) => flow.decide('approval', 'Approve A?', ['yes', 'no'])\n`)
    assert.equal((await w.person(`tmag-flow start ${p} --run c2`)).code, 5)
    assert.equal((await w.person('tmag-flow decide c2 approval yes')).code, 0)
    planFile('approve', `export default (flow) => flow.decide('approval', 'Approve B?', ['yes', 'no'])\n`)
    const again = await w.person('tmag-flow resume c2')
    assert.equal(again.code, 5, again.out)
    assert.match(again.out, /approval halted: Approve B\?/)
    assert.equal((await w.person('tmag-flow decide c2 approval no')).code, 0)
    const done = await w.person('tmag-flow resume c2')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'no')
  })

  it('keeps a decision for the invocation that carries it out', async () => {
    w = new World()
    const p = planFile(
      'later',
      `export default async (flow) => {\n  await new Promise((done) => setTimeout(done, 300))\n  return flow.decide('approval', 'Approve A?', ['yes', 'no'])\n}\n`,
    )
    assert.equal((await w.person(`tmag-flow start ${p} --run k2`)).code, 5)
    assert.equal((await w.person('tmag-flow decide k2 approval yes')).code, 0)
    assert.equal((await w.person('tmag-flow resume k2 --for 1ms')).code, 4)
    const done = await w.person('tmag-flow resume k2')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'yes')
    assert.equal(records('k2', 'use').length, 0, 'the done that carries a decision out is its only record')
  })

  it('takes the reply of a turn that ended after its timeout, decided to wait, and leaves later work alone', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const halted = await w.person(`tmag-flow start ${plan('one')} --run n2 --ask-timeout 1s`)
    assert.equal(halted.code, 5, halted.out)
    assert.match(halted.out, /next: tmag-flow decide n2 impl-1 wait\|abandon, then/)
    const [impl] = bound('n2', 'impl')
    assert.equal((await w.person('tmag-flow decide n2 impl-1 wait')).code, 0)
    end(impl as string)
    await waitFor(() => stateOf(impl as string) === 'idle', 10_000, 'the turn to end')
    assert.equal((await w.person(`tmag send ${impl} - <<'EOF'\nother work\nEOF`)).code, 0)
    const done = await w.person('tmag-flow resume n2')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'answered: one done')
    assert.equal(stateOf(impl as string), 'working', 'the other work goes on')
  })

  it('halts on a send its runner died during, and takes the turn once it shows', async () => {
    w = new World()
    const ready = join(w.dir, 'ready')
    const go = join(w.dir, 'go')
    const late = join(w.dir, 'late-tmag')
    writeFileSync(
      late,
      `#!/bin/sh\nif [ "$1" = send ]; then touch ${ready}; while [ ! -f ${go} ]; do sleep .1; done; exec ${join(w.bin, 'tmag')} "$@"; fi\nexec ${join(w.bin, 'tmag')} "$@"\n`,
      { mode: 0o755 },
    )
    const bg = w.background(`TMAG_FLOW_TMAG=${late} tmag-flow start ${plan('one')} --run o2`)
    await waitFor(() => existsSync(ready), 30_000, 'the send to be held')
    process.kill(runner('o2'), 'SIGKILL')
    await waitFor(bg.done, 10_000, 'the runner to die')
    const halted = await w.person('tmag-flow resume o2')
    assert.equal(halted.code, 5, halted.out)
    assert.match(
      halted.out,
      /impl-1 halted: a send of this prompt to impl \(\w+\) was under way when its runner stopped/,
    )
    assert.match(halted.out, /next: tmag-flow resume o2 looks again; or tmag-flow decide o2 impl-1 rerun\|abandon/)
    const [impl] = bound('o2', 'impl')
    writeFileSync(go, '')
    await waitFor(() => sends(impl as string) === 1, 30_000, 'the held send to deliver')
    const done = await w.person('tmag-flow resume o2')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'answered: one done')
    assert.equal(sends(impl as string), 1)
  })

  it('carries out a decision to rerun a changed step though its runner died right after taking it up', async () => {
    w = new World()
    const cmd = join(w.dir, 'cmd')
    writeFileSync(cmd, 'echo one')
    const p = planFile(
      'gated',
      `import { readFileSync } from 'node:fs'\nexport default async (flow) => {\n  const r = await flow.gate('check', readFileSync(${JSON.stringify(cmd)}, 'utf8'))\n  return [r.output.trim(), await flow.decide('go-on', 'go on?', ['go'])]\n}\n`,
    )
    assert.equal((await w.person(`tmag-flow start ${p} --run v2`)).code, 5)
    writeFileSync(cmd, 'echo two')
    const diverged = await w.person('tmag-flow resume v2')
    assert.equal(diverged.code, 5, diverged.out)
    assert.match(diverged.out, /check halted: the plan now describes this gate differently/)
    assert.equal((await w.person('tmag-flow decide v2 check rerun')).code, 0)
    const crash = join(w.dir, 'crash.cjs')
    writeFileSync(
      crash,
      `const fs = require('node:fs')\nconst { syncBuiltinESMExports } = require('node:module')\nconst write = fs.writeSync\nconst sync = fs.fsyncSync\nlet cut\nfs.writeSync = (fd, data, ...rest) => {\n  const n = write(fd, data, ...rest)\n  if (typeof data === 'string' && data.startsWith('{"t":"reset"')) cut = fd\n  return n\n}\nfs.fsyncSync = (fd) => {\n  sync(fd)\n  if (fd === cut) process.kill(process.pid, 'SIGKILL')\n}\nsyncBuiltinESMExports()\n`,
    )
    assert.equal((await w.person(`NODE_OPTIONS=--require=${crash} tmag-flow resume v2`)).code, 137)
    const again = await w.person('tmag-flow resume v2')
    assert.equal(again.code, 5, again.out)
    assert.match(again.out, /go-on halted: go on\?/)
    const check = records('v2', 'done').findLast((r) => r.key === 'check')?.result as { output: string } | undefined
    assert.match(check?.output ?? '', /two/)
  })

  it('gives no prompt again that a later invocation sent, when tmag cannot show the turns for a while', async () => {
    w = new World()
    const delivered = join(w.dir, 'delivered')
    const holds = join(w.dir, 'holds-tmag')
    writeFileSync(
      holds,
      `#!/bin/sh\nif [ "$1" = send ]; then ${join(w.bin, 'tmag')} "$@"; touch ${delivered}; exec sleep 600; fi\nexec ${join(w.bin, 'tmag')} "$@"\n`,
      { mode: 0o755 },
    )
    const flaky = join(w.dir, 'flaky-tmag')
    const once = join(w.dir, 'read-failed')
    writeFileSync(
      flaky,
      `#!/bin/sh\nif [ "$1" = read ] && [ ! -f ${once} ]; then touch ${once}; echo 'tmag read: a moment' >&2; exit 1; fi\nexec ${join(w.bin, 'tmag')} "$@"\n`,
      { mode: 0o755 },
    )
    assert.equal((await w.person(`TMAG_FLOW_TMAG=${failing()} tmag-flow start ${plan('one')} --run x2`)).code, 1)
    const bg = w.background(`TMAG_FLOW_TMAG=${holds} tmag-flow resume x2`)
    await waitFor(() => existsSync(delivered), 30_000, 'the second invocation to deliver')
    process.kill(runner('x2'), 'SIGKILL')
    await waitFor(bg.done, 10_000, 'the runner to die')
    const [impl] = bound('x2', 'impl')
    const unsure = await w.person(`TMAG_FLOW_TMAG=${flaky} tmag-flow resume x2`)
    assert.equal(unsure.code, 5, unsure.out)
    assert.equal(sends(impl as string), 1, 'the prompt was given once')
    const done = await w.person('tmag-flow resume x2')
    assert.equal(done.code, 0, done.out)
    assert.equal(done.out.trim().split('\n').at(-1), 'answered: one done')
    assert.equal(sends(impl as string), 1)
  })

  it('gives its prompt once other work that kept the agent busy is done, decided to wait', async () => {
    w = new World()
    w.tmux('set-environment', '-g', 'FAKE_SLOW', ' pi ')
    const go = join(w.dir, 'go')
    const p = planFile(
      'busy',
      `export default async (flow) => {
  const impl = flow.role('impl', { agent: 'impl' })
  await flow.bind(impl)
  await flow.gate('go', 'while [ ! -f ${go} ]; do sleep .1; done')
  return (await flow.ask('impl-1', impl, 'Work on it.\\nrunner work', { timeout: '5s' })).text
}\n`,
    )
    const bg = w.background(`tmag-flow start ${p} --run i2`)
    await waitFor(() => bound('i2', 'impl').length === 1, 30_000, 'the role to start')
    const [impl] = bound('i2', 'impl')
    assert.equal((await w.person(`tmag send ${impl} - <<'EOF'\nother work\nEOF`)).code, 0)
    writeFileSync(go, '')
    await waitFor(bg.done, 30_000, 'the run to halt')
    assert.equal(bg.code(), 5, readFileSync(bg.out, 'utf8'))
    assert.match(
      readFileSync(bg.out, 'utf8'),
      /stayed busy with other work for the whole timeout\nnext: tmag-flow decide i2 impl-1 wait\|abandon/,
    )
    assert.equal(sends(impl as string), 1, 'the prompt waited for the other work')
    assert.equal((await w.person('tmag-flow decide i2 impl-1 wait')).code, 0)
    const resumed = w.background('tmag-flow resume i2')
    end(impl as string)
    await waitFor(() => records('i2', 'delivered').length === 1, 30_000, 'the prompt to be given')
    end(impl as string)
    await waitFor(resumed.done, 30_000, 'the run to finish')
    assert.equal(resumed.code(), 0, readFileSync(resumed.out, 'utf8'))
    assert.equal(sends(impl as string), 2)
  })

  it('bounds the start of an agent that shows no session by the time an ask may take', async () => {
    w = new World()
    const stuck = join(w.dir, 'codex')
    writeFileSync(stuck, '#!/bin/sh\nexec sleep 600\n', { mode: 0o755 })
    w.tmux('set-option', '-g', '@tmag-start-impl', stuck)
    const began = Date.now()
    const r = await w.person(`tmag-flow start ${plan('one')} --run s2 --ask-timeout 1s`)
    assert.equal(r.code, 1, r.out)
    assert.match(r.out, /impl: its agent still shows no session in pane \S+\nnext: the person answers what pane/)
    assert.ok(Date.now() - began < 40_000, `the start took ${Date.now() - began}ms`)
  })

  it('starts no more sessions than its limit, roles starting at once', async () => {
    w = new World()
    const r = await w.person(`tmag-flow start ${plan('pair')} --run g1 --agents 1`)
    assert.equal(r.code, 1, r.out)
    assert.match(r.out, /the run has started 1 sessions, its limit\nnext: resume it with --agents 2/)
    assert.equal(records('g1', 'role.intent').length, 1)
  })

  it('runs the plans that ship, by their names', async () => {
    w = new World()
    const spec = join(w.dir, 'units.json')
    writeFileSync(
      spec,
      JSON.stringify({
        units: [
          { name: 'one', task: 'Do one.', accept: ['true'] },
          { name: 'two', task: 'Do two.', accept: ['git rev-parse --abbrev-ref HEAD | grep -qx feat/u'] },
        ],
      }),
    )
    const u = await w.person(
      `tmag-flow start units --run u1 --arg units=${spec} --arg implementer=impl --arg branch=feat/u --json`,
    )
    assert.equal(u.code, 0, u.out)
    const doc = JSON.parse(u.out.trim().split('\n').at(-1) as string) as {
      result: { units: { unit: string; result: string; revision: string }[] }
    }
    assert.deepEqual(
      doc.result.units.map((x) => [x.unit, x.result]),
      [
        ['one', 'passed'],
        ['two', 'passed'],
      ],
    )
    const head = revisionAt('feat/u')
    for (const unit of doc.result.units) assert.match(unit.revision, head, unit.unit)

    const r = await w.person('tmag-flow start review --run v1 --arg dir=. --arg reviewers=sol')
    assert.equal(r.code, 5, r.out)
    assert.match(r.out, /round-1 halted: sol \(unclear\): no report\. Fix the findings/)
    assert.equal((await w.person('tmag-flow decide v1 round-1 stop')).code, 0)
    const done = await w.person('tmag-flow resume v1 --json')
    assert.equal(done.code, 0, done.out)
    const review = JSON.parse(done.out.trim().split('\n').at(-1) as string) as {
      result: { passed: boolean; revision: string }
    }
    assert.equal(review.result.passed, false)
    assert.match(review.result.revision, revisionAt('HEAD'))
  })

  it('runs a unit named review and starts its final reviewer', async () => {
    w = new World()
    const spec = join(w.dir, 'units.json')
    writeFileSync(spec, JSON.stringify({ units: [{ name: 'review', task: 'Do the work.', accept: ['true'] }] }))
    const run = await w.person(
      `tmag-flow start units --run u2 --arg units=${spec} --arg implementer=impl --arg branch=feat/review --arg reviewers=sol --arg rounds=1 --json`,
    )
    assert.doesNotMatch(run.out, /step .* is used twice/)
    assert.equal(run.code, 0, run.out)
    const { result } = JSON.parse(run.out.trim().split('\n').at(-1) as string) as {
      result: {
        units: { unit: string; result: string }[]
        review: { passed: boolean; round: number; revision: string }
      }
    }
    assert.equal(result.units[0]?.unit, 'review')
    assert.equal(result.units[0]?.result, 'passed')
    assert.equal(bound('u2', 'sol').length, 1)
    assert.equal(result.review.round, 1)
    assert.equal(result.review.passed, false)
    assert.match(result.review.revision, revisionAt('feat/review'))
  })

  it('is the agent whose tool runs it', async () => {
    w = new World()
    // A lead agent whose tool runs tmag-flow: tmag proves its calls as the
    // lead's, so the run's prompts say who sent them.
    const shim = join(w.dir, 'shim')
    writeFileSync(
      shim,
      `#!/bin/sh\ncase $1 in flow) shift; exec ${join(w.bin, 'tmag-flow')} "$@";; esac\nexec ${join(w.bin, 'tmag')} "$@"\n`,
      { mode: 0o755 },
    )
    const leadDir = join(w.dir, 'lead')
    mkdirSync(leadDir)
    writeFileSync(
      join(leadDir, 'claude'),
      `#!/bin/sh\nTMAG_BIN=${shim} exec ${join(w.bin, 'agents', 'claude')} "$@"\n`,
      {
        mode: 0o755,
      },
    )
    w.tmux('set-option', '-g', '@tmag-start-lead', join(leadDir, 'claude'))
    const started = await w.person('tmag start lead --json')
    assert.equal(started.code, 0, started.out)
    const lead = (JSON.parse(started.out.trim().split('\n')[0] as string) as { session: { id: string } }).session.id
    const asked = await w.person(
      `tmag send ${lead} --wait --timeout 90s -- "tmag flow start ${plan('one')} --run a1"`,
      120_000,
    )
    assert.equal(asked.code, 0, asked.out)
    const inv = records('a1', 'invocation')[0] as { caller: { by?: { session: string } } }
    assert.equal(inv.caller.by?.session, lead)
    const [impl] = bound('a1', 'impl')
    const start = w.events(impl as string).find((e) => e.event === 'Start') as { by?: { session: string } }
    assert.equal(start.by?.session, lead)
    const turns = w.tmag('read', impl as string, '--turns', '5').docs[0]?.turns as { prompts: { text: string }[] }[]
    const text = turns.at(-1)?.prompts[0]?.text ?? ''
    assert.match(
      text,
      new RegExp(
        `^\\[tmag\\] From claude ${lead}, another agent\\. Answer in your final message\\.\\ntmag-flow run a1, step impl-1, attempt 1\\n`,
      ),
    )
  })

  it('names the windows it starts, when tmag can', async (t) => {
    w = new World()
    if (!/tmag start\b[^\n]*--name NAME/.test(execFileSync(join(w.bin, 'tmag'), ['--help'], { encoding: 'utf8' }))) {
      t.skip('this tmag names no windows')
      return
    }
    const r = await w.person(`tmag-flow start ${plan('pair')} --run n1`)
    assert.equal(r.code, 0, r.out)
    const windows = w.tmux('list-windows', '-F', '#{window_name}').split('\n')
    for (const role of ['sol', 'astra']) {
      const name = records('n1', 'role.bound').find((x) => x.role === role)?.window as string
      assert.match(name, new RegExp(`^${role}-[g-z]{4}$`))
      assert.ok(windows.includes(name), `${name} in ${windows.join(' ')}`)
    }
  })
})
