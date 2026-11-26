import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { after, describe, it } from 'node:test'
import { verdictOf } from '../../plans/lib/review.ts'
import review from '../../plans/review.ts'
import units, { parseUnits } from '../../plans/units.ts'
import type { Answer, CommandResult, Flow, Role, RoleSpec } from '../../src/types.ts'

// A flow whose agents, commands and deciders follow a script, and that
// records every step the plan takes, as the runner would see them. A
// reviewer writes the report its prompt asks for unless its step is in
// unwritten, and the check for a report runs as the runner runs it, unless
// judged says what it found.
const made: string[] = []
after(() => {
  for (const dir of made) rmSync(dir, { recursive: true, force: true })
})

class Scripted {
  readonly steps: string[] = []
  readonly artifacts = made[made.push(mkdtempSync(join(tmpdir(), 'tmag-flow-plans-'))) - 1] as string
  readonly unwritten = new Set<string>()
  readonly judged = new Map<string, boolean>()
  readonly prompts = new Map<string, string>()
  readonly roles = new Map<string, RoleSpec>()
  readonly args: Record<string, string>
  readonly answer: (key: string) => string
  readonly gate: (key: string, command: string) => boolean
  readonly decide: (key: string) => string
  readonly files: Record<string, string>
  constructor(
    args: Record<string, string>,
    answer: (key: string) => string,
    gate: (key: string, command: string) => boolean,
    decide: (key: string) => string,
    files: Record<string, string> = {},
  ) {
    this.args = args
    this.answer = answer
    this.gate = gate
    this.decide = decide
    this.files = files
  }

  flow(): Flow {
    const project = '/project'
    return {
      run: 'r',
      dir: '/run',
      project,
      args: this.args,
      role: (name: string, spec: RoleSpec): Role => {
        this.roles.set(name, spec)
        return { name, spec }
      },
      bind: async (role: Role) => ({ session: `s-${role.name}`, agent: role.spec.agent, dir: `/tree/${role.name}` }),
      ask: async (key: string, role: Role, prompt: string): Promise<Answer> => {
        this.steps.push(`ask ${key} ${role.name}`)
        this.prompts.set(key, prompt)
        const text = this.answer(key)
        const report = /^Write your (?:full )?report.* to (\/\S+)\.$/m.exec(prompt)?.[1]
        if (report && !this.unwritten.has(key)) {
          mkdirSync(dirname(report), { recursive: true })
          writeFileSync(report, 'findings')
        }
        return { text, turn: 't', session: `s-${role.name}` }
      },
      gate: async (key: string, command: string): Promise<CommandResult> => {
        this.steps.push(`gate ${key}`)
        const ok = key.endsWith('/report')
          ? (this.judged.get(key) ?? spawnSync('/bin/sh', ['-c', command]).status === 0)
          : this.gate(key, command)
        return { ok, code: ok ? 0 : 1, output: ok ? '' : `${command} failed` }
      },
      exec: async () => {
        throw new Error('no exec in these plans')
      },
      decide: async (key: string) => {
        this.steps.push(`decide ${key}`)
        return this.decide(key)
      },
      freeze: async (key: string, path: string) => {
        this.steps.push(`freeze ${key}`)
        const content = this.files[path]
        if (content === undefined) throw new Error(`no file ${path}`)
        return content
      },
      path: (...segments: string[]) => {
        const path = join(this.artifacts, ...segments)
        mkdirSync(dirname(path), { recursive: true })
        return path
      },
      log: () => {},
    }
  }
}

const never = (key: string): string => {
  throw new Error(`no decision scripted for ${key}`)
}

describe('verdicts', () => {
  it('are the line a reply ends with', () => {
    assert.equal(verdictOf('report: /r.md\nVERDICT: pass\n'), 'pass')
    assert.equal(verdictOf('VERDICT: pass\nthen more\n**Verdict: FAIL**'), 'fail')
    assert.equal(verdictOf('Earlier assessment\nVERDICT: pass\nMy final assessment is inconclusive.'), 'unclear')
    assert.equal(verdictOf('VERDICT: pass, unless the tests are wrong'), 'unclear')
    assert.equal(verdictOf('looks fine to me'), 'unclear')
  })
})

describe('the review plan', () => {
  it('ends once every reviewer passes', async () => {
    const s = new Scripted(
      { dir: 'src', reviewers: 'sol,astra' },
      () => 'VERDICT: pass',
      () => true,
      never,
    )
    const out = (await review(s.flow())) as { passed: boolean; round: number }
    assert.deepEqual(out.passed, true)
    assert.equal(out.round, 1)
    assert.deepEqual(s.steps.sort(), [
      'ask review/astra/round-1 astra',
      'ask review/sol/round-1 sol',
      'gate review/astra/round-1/report',
      'gate review/sol/round-1/report',
    ])
    assert.equal(s.roles.get('sol')?.dir, '/project/src')
  })

  it('halts on a failure, and has who failed check the fixes against the response', async () => {
    const s: Scripted = new Scripted(
      { dir: '.', reviewers: 'sol,astra', brief: 'b.md' },
      (key) => (key === 'review/astra/round-1' ? 'VERDICT: fail' : 'VERDICT: pass'),
      () => true,
      () => {
        writeFileSync(join(s.artifacts, 'responses', 'round-1.md'), 'fixed one, left two: it holds')
        return 'recheck'
      },
      { 'b.md': 'the brief' },
    )
    const out = (await review(s.flow())) as { passed: boolean; round: number }
    assert.equal(out.passed, true)
    assert.equal(out.round, 2)
    assert.ok(s.steps.includes('decide round-1'))
    assert.ok(s.steps.includes('ask review/astra/round-2 astra'))
    assert.ok(!s.steps.includes('ask review/sol/round-2 sol'), 'who passed is not asked again')
    assert.match(s.prompts.get('review/astra/round-2') as string, /astra-round-1\.md\. Its findings were addressed/)
    assert.match(
      s.prompts.get('review/astra/round-2') as string,
      /why any finding was left as it is, is in \S+\/responses\/round-1\.md\. Check each fix, each reason/,
    )
    assert.match(s.prompts.get('review/sol/round-1') as string, /Brief:\nthe brief/)
  })

  it('gives each reviewer what every reviewer and it alone looks at first', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol,astra', focus: 'the journal', 'focus-astra': 'design it from scratch first' },
      () => 'VERDICT: pass',
      () => true,
      never,
    )
    await review(s.flow())
    assert.match(s.prompts.get('review/astra/round-1') as string, /Focus: the journal\ndesign it from scratch first\n/)
    assert.match(s.prompts.get('review/sol/round-1') as string, /Focus: the journal\nWrite your full report/)
  })

  it('passes no reviewer whose report is missing', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol', rounds: '1' },
      () => 'VERDICT: pass',
      () => true,
      never,
    )
    s.unwritten.add('review/sol/round-1')
    const out = (await review(s.flow())) as { passed: boolean; reviews: { verdict: string }[] }
    assert.equal(out.passed, false)
    assert.equal(out.reviews[0]?.verdict, 'unclear')
  })

  it('judges a review by the check for its report the run recorded, not by what the disk holds later', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol' },
      () => 'VERDICT: pass',
      () => true,
      () => 'stop',
    )
    s.judged.set('review/sol/round-1/report', false)
    const out = (await review(s.flow())) as { passed: boolean; reviews: { verdict: string; report?: string }[] }
    assert.equal(out.passed, false)
    assert.deepEqual(out.reviews[0], { reviewer: 'sol', verdict: 'unclear', session: 's-sol' })
    assert.ok(s.steps.includes('decide round-1'))
  })

  it('takes a directory where the report should be for no report', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol', rounds: '1' },
      () => 'VERDICT: pass',
      () => true,
      never,
    )
    s.unwritten.add('review/sol/round-1')
    mkdirSync(join(s.artifacts, 'reports', 'sol-round-1.md'), { recursive: true })
    const out = (await review(s.flow())) as { passed: boolean }
    assert.equal(out.passed, false)
  })

  it('ends with its last round, offering no recheck it would not run', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol', rounds: '2' },
      () => 'VERDICT: fail',
      () => true,
      (key) => (key === 'round-1' ? 'recheck' : never(key)),
    )
    const out = (await review(s.flow())) as { passed: boolean; round: number }
    assert.deepEqual([out.passed, out.round], [false, 2])
    assert.deepEqual(s.steps, [
      'ask review/sol/round-1 sol',
      'gate review/sol/round-1/report',
      'decide round-1',
      'ask review/sol/round-2 sol',
      'gate review/sol/round-2/report',
    ])
  })

  it('takes an abandoned review for no verdict, and asks that reviewer afresh', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol,astra' },
      (key) => {
        if (key === 'review/sol/round-1') throw Object.assign(new Error('step abandoned'), { name: 'Abandoned' })
        return key === 'review/astra/round-1' ? 'VERDICT: fail' : 'VERDICT: pass'
      },
      () => true,
      () => 'recheck',
    )
    const out = (await review(s.flow())) as { passed: boolean; round: number }
    assert.deepEqual([out.passed, out.round], [true, 2])
    assert.match(s.prompts.get('review/sol/round-2') as string, /^Review the work in /)
    assert.match(s.prompts.get('review/astra/round-2') as string, /^Your previous report is /)
  })

  it('stops when told to', async () => {
    const s = new Scripted(
      { dir: '.', reviewers: 'sol' },
      () => 'nothing to say',
      () => true,
      () => 'stop',
    )
    const out = (await review(s.flow())) as { passed: boolean; reviews: { verdict: string }[] }
    assert.equal(out.passed, false)
    assert.equal(out.reviews[0]?.verdict, 'unclear')
  })

  it('says which argument it lacks', async () => {
    const s = new Scripted(
      { reviewers: 'sol' },
      () => '',
      () => true,
      never,
    )
    await assert.rejects(review(s.flow()), /--arg dir=PATH/)
  })
})

const spec = JSON.stringify({
  units: [
    { name: 'parser', task: 'Write the parser.', accept: ['test parser'] },
    { name: 'printer', task: 'Write the printer.', accept: ['test printer', 'lint'] },
  ],
})

describe('the units plan', () => {
  it('runs each unit through its acceptance commands, sending failures back', async () => {
    let printerTries = 0
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'feat/x' },
      () => 'done',
      (_key, command) => command !== 'test printer' || ++printerTries > 1,
      never,
      { 'u.json': spec },
    )
    const out = (await units(s.flow())) as { units: { unit: string; tries: number }[] }
    assert.deepEqual(
      out.units.map((u) => [u.unit, u.tries]),
      [
        ['parser', 1],
        ['printer', 2],
      ],
    )
    assert.deepEqual(s.roles.get('impl'), { agent: 'pi', worktree: 'feat/x' })
    assert.ok(s.steps.includes('ask printer/fix-1 impl'))
    assert.match(s.prompts.get('printer/fix-1') as string, /\$ test printer {2}\(exit 1\)\ntest printer failed/)
    assert.match(s.prompts.get('printer/task') as string, /Unit 2 of 2: printer\.[\s\S]*\$ test printer\n {2}\$ lint/)
  })

  it('halts a unit that keeps failing, and skips it when told to', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', attempts: '2' },
      () => 'done',
      (_key, command) => command !== 'test parser',
      (key) => (key === 'parser/after-2' ? 'skip' : never(key)),
      { 'u.json': spec },
    )
    const out = (await units(s.flow())) as { units: { unit: string; result: string }[] }
    assert.deepEqual(
      out.units.map((u) => [u.unit, u.result]),
      [
        ['parser', 'skipped'],
        ['printer', 'passed'],
      ],
    )
  })

  it('has the reviewers check at the end, and the implementer fix what they find', async () => {
    let round = 0
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', reviewers: 'sol,astra' },
      (key) => {
        if (key.startsWith('review/sol/')) return 'VERDICT: pass'
        if (key.startsWith('review/astra/')) return ++round > 1 ? 'VERDICT: pass' : 'VERDICT: fail'
        return 'done'
      },
      () => true,
      never,
      { 'u.json': spec },
    )
    const out = (await units(s.flow())) as { review: { passed: boolean; round: number } }
    assert.deepEqual(out.review.passed, true)
    assert.equal(out.review.round, 2)
    assert.ok(s.steps.includes('ask review/fix-1 impl'))
    assert.ok(s.steps.includes('gate review/accept-1/try-1/accept-1'))
    const response = /is in (\S+)\. Check each fix/.exec(s.prompts.get('review/astra/round-2') as string)?.[1]
    assert.equal(readFileSync(response as string, 'utf8'), 'done')
    assert.equal(s.roles.get('astra')?.dir, '/tree/impl')
  })

  it('passes no review whose fixes fail the acceptance commands', async () => {
    let gates = 0
    let round = 0
    const one = JSON.stringify({ units: [{ name: 'one', task: 'Do it.', accept: ['test one'] }] })
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', reviewers: 'sol' },
      (key) => (key.startsWith('review/sol/') ? (++round > 1 ? 'VERDICT: pass' : 'VERDICT: fail') : 'done'),
      () => ++gates === 1,
      (key) => (key === 'review/accept-1/after-2' ? 'stop' : never(key)),
      { 'u.json': one },
    )
    const out = (await units(s.flow())) as { review: { passed: boolean } }
    assert.equal(out.review.passed, false)
    assert.deepEqual(
      s.steps.filter((step) => step.startsWith('ask') || step.startsWith('decide')),
      [
        'ask one/task impl',
        'ask review/sol/round-1 sol',
        'ask review/fix-1 impl',
        'ask review/accept-1/fix-1 impl',
        'decide review/accept-1/after-2',
      ],
    )
  })

  it('reads a units file and says what is wrong with one', () => {
    assert.equal(parseUnits(spec).length, 2)
    assert.throws(() => parseUnits('{'), /not JSON/)
    assert.throws(() => parseUnits('{"units":[]}'), /no "units" list/)
    assert.throws(() => parseUnits('{"units":[{"name":"a","task":"t","accept":[]}]}'), /needs "accept"/)
    assert.throws(
      () => parseUnits('{"units":[{"name":"a","task":"t","accept":["x"]},{"name":"a","task":"t","accept":["x"]}]}'),
      /named twice/,
    )
  })
})

describe('plans that ship', () => {
  it('describe themselves', async () => {
    for (const name of ['review', 'units']) {
      const mod = (await import(`../../plans/${name}.ts`)) as { description?: string }
      assert.ok(mod.description && mod.description.length < 120, name)
    }
  })
})
