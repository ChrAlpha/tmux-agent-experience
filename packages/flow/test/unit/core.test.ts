import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { appendFileSync, existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { hostname, tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, describe, it, type TestContext } from 'node:test'
import { formatDuration, parseDuration, after as timer } from '../../src/duration.ts'
import { FlowError } from '../../src/errors.ts'
import { Journal } from '../../src/journal.ts'
import { acquire } from '../../src/lock.ts'
import { hash } from '../../src/project.ts'
import { Runtime, windowName } from '../../src/runtime.ts'
import { type Result, short, Tmag } from '../../src/tmag.ts'

// scratch is a directory of a test's own, gone once the file's tests end.
const made: string[] = []
const scratch = () => {
  const dir = mkdtempSync(join(tmpdir(), 'tmag-flow-unit-'))
  made.push(dir)
  return dir
}
after(() => {
  for (const dir of made) rmSync(dir, { recursive: true, force: true })
})

describe('durations', () => {
  it('reads what tmag writes', () => {
    assert.equal(parseDuration('90s'), 90_000)
    assert.equal(parseDuration('1h30m'), 5_400_000)
    assert.equal(parseDuration('250ms'), 250)
    for (const bad of ['', '5', '5x', '1h 2m', '0s', '-3s']) assert.throws(() => parseDuration(bad), /duration/)
  })
  it('time what is longer than a timer of Node holds', async () => {
    let fired = false
    const t = timer(2 ** 31 + 1000, () => {
      fired = true
    })
    await new Promise((r) => setTimeout(r, 50))
    t.clear()
    assert.equal(fired, false)
  })

  it('writes what tmag reads, rounded up', () => {
    assert.equal(formatDuration(1), '1ms')
    assert.equal(formatDuration(1500), '2s')
    assert.equal(parseDuration(formatDuration(61_001)), 62_000)
  })
})

describe('the journal', () => {
  it('reads back what it appended, and ignores a line a crash cut short', () => {
    const dir = scratch()
    const j = Journal.open(dir)
    j.append({ t: 'error', message: 'one' })
    j.close()
    appendFileSync(join(dir, 'journal.jsonl'), '{"t":"err')
    const k = Journal.open(dir)
    assert.equal(k.records.length, 1)
    k.append({ t: 'error', message: 'two' })
    k.close()
    assert.equal(readFileSync(join(dir, 'journal.jsonl'), 'utf8').split('\n').length, 3, 'the torn line is gone')
    assert.deepEqual(
      Journal.open(dir).records.map((r) => (r as { message?: string }).message),
      ['one', 'two'],
    )
  })

  it('refuses a journal damaged in the middle', () => {
    const dir = scratch()
    writeFileSync(join(dir, 'journal.jsonl'), '{"t":"error","message":"one"}\nnot json\n{"t":"end"}\n')
    assert.throws(() => Journal.open(dir), /line 2 is not a journal record/)
  })

  it('settles a halt by a later done, and keeps one that came after a done', () => {
    const j = Journal.open(scratch())
    j.append({ t: 'done', key: 'a', kind: 'ask', fp: 'x', result: {} })
    const h = j.append({ t: 'halt', key: 'a', fp: 'z', kind: 'divergence', question: 'q', choices: ['rerun'] })
    assert.equal(j.halt('a')?.halt.seq, h.seq, 'a divergence comes after the done it questions')
    assert.deepEqual(
      j.pending().map((p) => p.key),
      ['a'],
    )
    const d = j.append({ t: 'decision', key: 'a', halt: h.seq, choice: 'rerun', caller: null })
    assert.equal(j.halt('a')?.decision?.seq, d.seq)
    assert.equal(j.pending().length, 0)
    j.append({ t: 'use', key: 'a', decision: d.seq })
    j.append({ t: 'reset', key: 'a' })
    assert.equal(j.done('a'), undefined, 'a reset step starts over')
    assert.equal(j.halt('a'), undefined)
    j.append({ t: 'halt', key: 'b', fp: 'y', kind: 'decision', question: 'q', choices: ['go'] })
    j.append({ t: 'done', key: 'b', kind: 'decide', fp: 'y', result: 'go' })
    assert.equal(j.halt('b'), undefined, 'a done after a halt settles it')
  })
})

describe('revision evidence', () => {
  const git = (cwd: string, ...args: string[]) => execFileSync('git', ['-C', cwd, ...args], { encoding: 'utf8' }).trim()

  function initialCommit(cwd: string): string {
    const author = ['-c', 'user.name=t', '-c', 'user.email=t@example.com']
    git(cwd, ...author, '-c', 'core.hooksPath=/dev/null', 'commit', '--no-gpg-sign', '--allow-empty', '-qm', 'initial')
    return git(cwd, 'rev-parse', 'HEAD')
  }

  function gitShim(t: TestContext, script: string): string {
    const bin = scratch()
    const shim = join(bin, 'git')
    const path = process.env.PATH
    writeFileSync(shim, `#!/bin/sh\n${script}\nPATH=\${PATH#*:} exec git "$@"\n`, { mode: 0o755 })
    process.env.PATH = `${bin}:${path ?? ''}`
    t.after(() => {
      if (path === undefined) delete process.env.PATH
      else process.env.PATH = path
    })
    return shim
  }

  function runner(dir: string, tmag = new Tmag('/bin/false')) {
    return new Runtime({
      journal: Journal.open(dir),
      tmag,
      run: 'r',
      dir,
      project: dir,
      args: {},
      budget: { concurrency: 1, agents: 1, askTimeout: 60_000 },
      deadline: undefined,
      caller: null,
      startName: false,
      say: () => {},
    })
  }

  it('samples cwd before a requested command and replays only the evidence it recorded', async (t) => {
    const dir = scratch()
    const cwd = scratch()
    const rt = runner(dir)
    t.after(() => rt.o.journal.close())
    const flow = rt.flow()
    // The test's temporary directory may itself be inside a worktree.
    const ceiling = process.env.GIT_CEILING_DIRECTORIES
    process.env.GIT_CEILING_DIRECTORIES = tmpdir()
    t.after(() => {
      if (ceiling === undefined) delete process.env.GIT_CEILING_DIRECTORIES
      else process.env.GIT_CEILING_DIRECTORIES = ceiling
    })
    assert.equal('revision' in (await flow.gate('outside', 'true', { cwd, revision: true })), false)
    git(cwd, 'init', '-q')
    assert.equal('revision' in (await flow.gate('unborn', 'true', { cwd, revision: true })), false)
    const head = initialCommit(cwd)
    assert.match(head, /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/)
    const clean = await flow.gate('g', 'true', { cwd, revision: true })
    assert.equal(clean.revision, head)
    assert.deepEqual(rt.o.journal.done('g')?.result, clean)
    assert.equal('revision' in (await flow.gate('n', 'true', { cwd })), false)
    assert.equal((await flow.exec('write', 'touch changed', { cwd, revision: true })).revision, head)
    assert.equal((await flow.gate('dirty', 'true', { cwd, revision: true })).revision, `${head} (dirty)`)
    rt.o.journal.close()

    const resumed = runner(dir)
    t.after(() => resumed.o.journal.close())
    const replay = resumed.flow()
    assert.equal((await replay.gate('g', 'true', { cwd, revision: true })).revision, head)
    assert.equal('revision' in (await replay.gate('n', 'true', { cwd, revision: true })), false)
  })

  it('reads HEAD, tracked changes and hidden untracked files with one Git status per sample', async (t) => {
    const cwd = scratch()
    git(cwd, 'init', '-q')
    const tracked = join(cwd, 'tracked')
    writeFileSync(tracked, 'initial\n')
    git(cwd, 'add', 'tracked')
    const head = initialCommit(cwd)
    git(cwd, 'config', 'status.showUntrackedFiles', 'no')
    const bare = scratch()
    git(cwd, 'clone', '--bare', '-q', '.', bare)
    const shim = gitShim(t, 'echo "$*" >> "$0.calls"')
    const rt = runner(scratch())
    t.after(() => rt.o.journal.close())
    const flow = rt.flow()
    assert.equal((await flow.gate('clean', 'true', { cwd, revision: true })).revision, head)
    const untracked = join(cwd, 'input')
    writeFileSync(untracked, 'new\n')
    assert.equal((await flow.gate('untracked', 'true', { cwd, revision: true })).revision, `${head} (dirty)`)
    rmSync(untracked)
    writeFileSync(tracked, 'changed\n')
    assert.equal((await flow.gate('tracked', 'true', { cwd, revision: true })).revision, `${head} (dirty)`)
    assert.equal('revision' in (await flow.gate('bare', 'true', { cwd: bare, revision: true })), false)
    assert.equal(
      readFileSync(`${shim}.calls`, 'utf8'),
      'status --porcelain=v2 --branch --untracked-files=all\n'.repeat(4),
    )
  })

  it('resumes an exec stopped during its Git status probe without asking for a decision', async (t) => {
    const cwd = scratch()
    git(cwd, 'init', '-q')
    const head = initialCommit(cwd)
    const dir = scratch()
    const rt = runner(dir)
    t.after(() => rt.o.journal.close())
    const shim = gitShim(t, 'if [ "$1" = status ]; then touch "$0.started"; sleep 30; fi')
    const command = 'echo ran >> marker'
    const pending = rt.flow().exec('write', command, { cwd, revision: true })
    try {
      await t.waitFor(() => assert.ok(existsSync(`${shim}.started`)), { timeout: 5000 })
    } finally {
      rt.stop('budget')
      await rt.settle()
    }
    await assert.rejects(pending, { name: 'Stop', reason: 'budget' })
    assert.equal(rt.o.journal.intent('write'), undefined)
    const marker = join(cwd, 'marker')
    assert.equal(existsSync(marker), false)
    rt.o.journal.close()
    rmSync(shim)

    const resumed = runner(dir)
    t.after(() => resumed.o.journal.close())
    const result = await resumed.flow().exec('write', command, { cwd, revision: true })
    assert.equal(result.ok, true)
    assert.equal(result.revision, head)
    assert.equal(readFileSync(marker, 'utf8'), 'ran\n')
    assert.equal(resumed.o.journal.attempts('write'), 1)
  })

  it('uses only the final send when a delivered ask resumes, even when its revision is absent', async (t) => {
    const key = 'review'
    const session = 's'
    const prompt = 'Review the files.'
    const tag = 'tmag-flow run r, step review, attempt 1'
    const fp = hash('ask', 'reviewer', prompt)
    const tmag = new Tmag('/bin/false')
    tmag.run = async ([verb, , mode]) => {
      assert.ok(verb === 'inspect' || verb === 'read', `unexpected tmag ${verb}`)
      const doc =
        mode === '--turns'
          ? { session: { state: 'idle' }, turns: [{ id: 't', prompts: [{ text: tag }], reply: 'done' }] }
          : {}
      return { code: 0, docs: [doc], stdout: '', stderr: '', error: undefined, message: undefined }
    }

    for (const revision of [undefined, `${'b'.repeat(40)} (dirty)`]) {
      const dir = scratch()
      const j = Journal.open(dir)
      j.append({ t: 'intent', key, kind: 'ask', fp, attempt: 1, session, tag })
      j.append({ t: 'send', key, attempt: 1, revision: 'a'.repeat(40) })
      j.append({ t: 'send.end', key, attempt: 1, code: 3 })
      j.append({ t: 'send', key, attempt: 1, ...(revision ? { revision } : {}) })
      j.append({ t: 'send.end', key, attempt: 1, code: 0 })
      j.append({ t: 'delivered', key, attempt: 1, session })
      j.close()
      const rt = runner(dir, tmag)
      t.after(() => rt.o.journal.close())
      const flow = rt.flow()
      const answer = await flow.ask(key, flow.role('reviewer', { agent: 'codex' }), prompt, { revision: true })
      assert.deepEqual(answer, { text: 'done', turn: 't', session, ...(revision ? { revision } : {}) })
      assert.deepEqual(rt.o.journal.done(key)?.result, answer)
    }
  })
})

describe('the run lock', () => {
  it('lets one process advance a run', () => {
    const dir = scratch()
    const held = acquire(dir, 'run x')
    assert.throws(() => acquire(dir, 'run x'), /run x is advancing in process \d+/)
    held.release()
    acquire(dir, 'run x').release()
  })

  it('takes over the lock of a process that ended', async () => {
    const dir = scratch()
    const child = spawn('sh', ['-c', 'exit 0'])
    await new Promise((r) => child.on('close', r))
    writeFileSync(
      join(dir, 'lock.1'),
      JSON.stringify({ pid: child.pid, start: 'then', host: (await import('node:os')).hostname() }),
    )
    acquire(dir, 'run x').release()
    assert.deepEqual(readdirSync(dir), ['lock.2'])
  })

  it('is held whatever time zone and locale each runner has', async () => {
    const dir = scratch()
    const lock = new URL('../../src/lock.ts', import.meta.url).pathname
    const take = (env: Record<string, string>, hold: boolean) => {
      const code = `
        const { acquire } = await import(${JSON.stringify(lock)})
        try { acquire(${JSON.stringify(dir)}, 'run x') } catch (e) { process.stdout.write(e.message); process.exit(1) }
        process.stdout.write('held')
        ${hold ? 'setTimeout(() => {}, 10_000)' : ''}
      `
      const child = spawn(process.execPath, ['--input-type=module', '-e', code], { env: { ...process.env, ...env } })
      let out = ''
      child.stdout.on('data', (b) => (out += b))
      return { child, out: () => out, done: new Promise<number | null>((r) => child.on('close', r)) }
    }
    const first = take({ TZ: 'Asia/Tokyo', LC_ALL: 'C' }, true)
    for (let i = 0; i < 100 && first.out() !== 'held'; i++) await new Promise((r) => setTimeout(r, 50))
    assert.equal(first.out(), 'held')
    const second = take({ TZ: 'America/Los_Angeles', LANG: 'de_DE.UTF-8' }, false)
    assert.equal(await second.done, 1)
    assert.match(second.out(), /run x is advancing in process \d+/)
    first.child.kill()
    await first.done
  })

  it('is held by one process at a time when many take it at once', async () => {
    const dir = scratch()
    writeFileSync(join(dir, 'lock.1'), JSON.stringify({ pid: 999999999, start: 'then', host: hostname() }))
    const lock = new URL('../../src/lock.ts', import.meta.url).pathname
    // Each takes the lock again and again, and while it holds it, holds a
    // file of its own that no other may hold.
    const contender = `
      const { acquire } = await import(${JSON.stringify(lock)})
      const { writeFileSync, unlinkSync } = await import('node:fs')
      const at = Number(process.argv[1])
      while (Date.now() < at) {}
      let held = 0
      for (let i = 0; i < 80; i++) {
        let l
        try { l = acquire(${JSON.stringify(dir)}, 'run x') } catch { continue }
        try { writeFileSync(${JSON.stringify(join(dir, 'holding'))}, '', { flag: 'wx' }) } catch { process.exit(3) }
        held++
        unlinkSync(${JSON.stringify(join(dir, 'holding'))})
        l.release()
      }
      process.stdout.write(String(held))
    `
    const at = Date.now() + 1500
    const runs = Array.from({ length: 8 }, () => {
      const child = spawn(process.execPath, ['--input-type=module', '-e', contender, String(at)])
      let out = ''
      child.stdout.on('data', (b) => (out += b))
      return new Promise<{ code: number | null; held: number }>((r) =>
        child.on('close', (code) => r({ code, held: Number(out || 0) })),
      )
    })
    const got = await Promise.all(runs)
    assert.deepEqual(
      got.map((g) => g.code),
      got.map(() => 0),
    )
    assert.ok(got.reduce((n, g) => n + g.held, 0) > 0)
  })
})

describe('window names', () => {
  it('are never taken for a session id', () => {
    for (let i = 0; i < 200; i++) {
      const name = windowName('dead', `run-${i}`, 1 + (i % 3))
      assert.match(name, /^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$/)
      assert.doesNotMatch(name, /^[0-9a-f-]+$/)
    }
    assert.equal(windowName('impl', 'r', 1), windowName('impl', 'r', 1))
  })
})

describe('session ids', () => {
  it('are shown by 8 characters, or as many as tell them apart', () => {
    const sol = '01a218ba-bdcd-7e83-861b-602aeb7d5f1e'
    const astra = '01a218ba-c97f-7830-8126-10d0a894cf82'
    assert.equal(short(sol), '01a218ba')
    assert.equal(short(sol, [sol, astra]), '01a218ba-b')
    assert.equal(short(astra, [sol, astra]), '01a218ba-c')
    assert.equal(short(sol, ['0b75549a-2e51-44c3-a042-b1c52933ef87']), '01a218ba')
  })
})

describe('a role whose agent shows no session yet', () => {
  it('is waited for as long as its ask may take, and then says who answers it', async () => {
    const dir = scratch()
    const asked: string[] = []
    const result = (code: number, doc: Record<string, unknown>): Result => ({
      code,
      docs: [doc],
      stdout: '',
      stderr: '',
      error: (doc.error as { code?: string } | undefined)?.code,
      message: undefined,
    })
    // tmag as it answers a start whose agent sits at a trust dialog.
    const tmag = {
      run: async (args: string[]) => {
        asked.push(args[0] as string)
        if (args[0] === 'start') return result(1, { error: { code: 'not_shown' }, pane: { server: '/s', id: '%1' } })
        if (args[0] === 'wait') {
          await new Promise((r) => setTimeout(r, 20))
          return result(4, { error: { code: 'timeout' } })
        }
        throw new Error(`tmag ${args.join(' ')} was not expected`)
      },
    } as unknown as Tmag
    const rt = new Runtime({
      journal: Journal.open(dir),
      tmag,
      run: 'r',
      dir,
      project: dir,
      args: {},
      budget: { concurrency: 1, agents: 4, askTimeout: 60_000 },
      deadline: undefined,
      caller: null,
      startName: false,
      say: () => {},
    })
    const flow = rt.flow()
    const impl = flow.role('impl', { agent: 'codex' })
    await assert.rejects(
      flow.ask('k', impl, 'Do it.', { timeout: '300ms' }),
      (e: unknown) =>
        e instanceof FlowError &&
        /impl: its agent still shows no session in pane \/s:%1/.test(e.message) &&
        e.next === 'the person answers what pane /s:%1 shows, then tmag-flow resume r',
    )
    await rt.settle()
    assert.equal(asked.filter((a) => a === 'start').length, 1)
    assert.ok(asked.filter((a) => a === 'wait').length > 2)
  })

  it('keeps its pane when tmag cannot say what the pane shows', async () => {
    const dir = scratch()
    const asked: string[] = []
    const result = (code: number, doc: Record<string, unknown>): Result => ({
      code,
      docs: [doc],
      stdout: '',
      stderr: 'tmag wait: ambiguous target\n',
      error: (doc.error as { code?: string } | undefined)?.code,
      message: undefined,
    })
    const tmag = {
      run: async (args: string[]) => {
        asked.push(args[0] as string)
        if (args[0] === 'start') return result(1, { error: { code: 'not_shown' }, pane: { server: '/s', id: '%1' } })
        if (args[0] === 'wait') return result(1, { error: { code: 'ambiguous' } })
        throw new Error(`tmag ${args.join(' ')} was not expected`)
      },
    } as unknown as Tmag
    const journal = Journal.open(dir)
    const rt = new Runtime({
      journal,
      tmag,
      run: 'r',
      dir,
      project: dir,
      args: {},
      budget: { concurrency: 1, agents: 4, askTimeout: 60_000 },
      deadline: undefined,
      caller: null,
      startName: false,
      say: () => {},
    })
    const flow = rt.flow()
    await assert.rejects(
      flow.ask('k', flow.role('impl', { agent: 'codex' }), 'Do it.'),
      (e: unknown) =>
        e instanceof FlowError && /impl: waiting for pane \/s:%1: tmag wait: ambiguous target/.test(e.message),
    )
    await rt.settle()
    assert.equal(asked.filter((a) => a === 'start').length, 1)
    assert.equal(journal.all('role.gone').length, 0, "the pane is still the role's")
  })
})
