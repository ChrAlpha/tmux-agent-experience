import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { after, describe, it, type TestContext } from 'node:test'
import { revisionsOf, verdictOf } from '../../plans/lib/review.ts'
import review from '../../plans/review.ts'
import units, { parseUnits } from '../../plans/units.ts'
import { Journal } from '../../src/journal.ts'
import { runsDirName } from '../../src/project.ts'
import { Runtime } from '../../src/runtime.ts'
import { Tmag } from '../../src/tmag.ts'
import type { Answer, CommandResult, Flow, Role, RoleSpec } from '../../src/types.ts'

// A flow whose agents, commands and deciders follow a script, and that
// records every step the plan takes, as the runner would see them. A
// reviewer writes the report its prompt asks for unless its step is in
// unwritten. A gate can replay a scripted result; otherwise the check for a
// report runs as the runner runs it. Asks and gates can return a revision.
const made: string[] = []
after(() => {
  for (const dir of made) rmSync(dir, { recursive: true, force: true })
})

class Scripted {
  readonly steps: string[] = []
  readonly artifacts = made[made.push(mkdtempSync(join(tmpdir(), 'tmag-flow-plans-'))) - 1] as string
  readonly unwritten = new Set<string>()
  readonly gateResults = new Map<string, CommandResult>()
  readonly revisions = new Map<string, string>()
  readonly prompts = new Map<string, string>()
  readonly roles = new Map<string, RoleSpec>()
  readonly args: Record<string, string>
  readonly answer: (key: string) => string
  readonly gate: (key: string, command: string) => boolean
  readonly decide: (key: string, question: string) => string
  readonly files: Record<string, string>
  constructor(
    args: Record<string, string>,
    answer: (key: string) => string,
    gate: (key: string, command: string) => boolean,
    decide: (key: string, question: string) => string,
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
    const keys = new Set<string>()
    const record = (key: string, step: string) => {
      assert.ok(!keys.has(key), `step ${key} is used twice in this run`)
      keys.add(key)
      this.steps.push(step)
    }
    const evidence = (key: string, requested?: boolean) => {
      const revision = requested ? this.revisions.get(key) : undefined
      return revision ? { revision } : {}
    }
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
      ask: async (key, role, prompt, options): Promise<Answer> => {
        record(key, `ask ${key} ${role.name}`)
        this.prompts.set(key, prompt)
        const text = this.answer(key)
        const report = /^Write your (?:full )?report.* to (\/\S+)\.$/m.exec(prompt)?.[1]
        if (report && !this.unwritten.has(key)) {
          mkdirSync(dirname(report), { recursive: true })
          writeFileSync(report, 'findings')
        }
        return { text, turn: 't', session: `s-${role.name}`, ...evidence(key, options?.revision) }
      },
      gate: async (key, command, options): Promise<CommandResult> => {
        record(key, `gate ${key}`)
        const recorded = this.gateResults.get(key)
        if (recorded) return recorded
        const ok = key.endsWith('/report')
          ? spawnSync('/bin/sh', ['-c', command]).status === 0
          : this.gate(key, command)
        return { ok, code: ok ? 0 : 1, output: ok ? '' : `${command} failed`, ...evidence(key, options?.revision) }
      },
      exec: async (key) => {
        record(key, `exec ${key}`)
        throw new Error('no exec in these plans')
      },
      decide: async (key: string, question: string) => {
        record(key, `decide ${key}`)
        return this.decide(key, question)
      },
      freeze: async (key: string, path: string) => {
        record(key, `freeze ${key}`)
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

const revisionA = 'a'.repeat(40)
const revisionB = `${'b'.repeat(40)} (dirty)`

describe('revision summaries', () => {
  it('keeps distinct evidence in step order, including unavailable samples', () => {
    assert.equal(revisionsOf([]), 'revision unavailable')
    assert.equal(revisionsOf([undefined, undefined]), 'revision unavailable')
    assert.equal(revisionsOf([revisionA, revisionA]), revisionA)
    assert.equal(
      revisionsOf([revisionB, undefined, revisionA, revisionB, undefined]),
      `mixed (${revisionB}; revision unavailable; ${revisionA})`,
    )
  })
})

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
  it('keeps the 0.1.0 halt question byte for byte when every reviewer lacks a revision', async () => {
    let question = ''
    const s = new Scripted(
      { dir: 'src', reviewers: 'sol,astra' },
      () => 'VERDICT: fail',
      () => true,
      (_key, text) => {
        question = text
        return 'stop'
      },
    )
    assert.equal((await review(s.flow())).revision, 'revision unavailable')
    const sol = join(s.artifacts, 'reports', 'sol-round-1.md')
    const astra = join(s.artifacts, 'reports', 'astra-round-1.md')
    const response = join(s.artifacts, 'responses', 'round-1.md')
    assert.equal(
      question,
      `sol (fail): ${sol}; astra (fail): ${astra}. Fix the findings in /project/src, write what you fixed and why you left any finding as it is to ${response}, then recheck; or stop.`,
    )
  })

  it('ends once every reviewer passes', async () => {
    const s = new Scripted(
      { dir: 'src', reviewers: 'sol,astra' },
      () => 'VERDICT: pass',
      () => true,
      never,
    )
    s.revisions.set('review/sol/round-1', revisionA)
    s.revisions.set('review/astra/round-1', revisionA)
    const out = await review(s.flow())
    assert.deepEqual(out.passed, true)
    assert.equal(out.round, 1)
    assert.equal(out.revision, revisionA)
    assert.deepEqual(
      out.reviews.map((r) => r.revision),
      [revisionA, revisionA],
    )
    for (const prompt of s.prompts.values()) assert.doesNotMatch(prompt, /The work is (?:now )?at /)
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
      (_key, question) => {
        assert.ok(question.includes(`Fix the findings in /project (mixed (${revisionA}; revision unavailable))`))
        writeFileSync(join(s.artifacts, 'responses', 'round-1.md'), 'fixed one, left two: it holds')
        return 'recheck'
      },
      { 'b.md': 'the brief' },
    )
    s.revisions.set('review/sol/round-1', revisionA)
    s.revisions.set('review/astra/round-2', revisionB)
    const out = await review(s.flow())
    assert.equal(out.passed, true)
    assert.equal(out.round, 2)
    assert.equal(out.revision, `mixed (${revisionA}; ${revisionB})`)
    assert.deepEqual(
      out.reviews.map((r) => [r.reviewer, r.revision]),
      [
        ['sol', revisionA],
        ['astra', revisionB],
      ],
    )
    for (const prompt of s.prompts.values()) assert.doesNotMatch(prompt, /The work is (?:now )?at /)
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
    s.gateResults.set('review/sol/round-1/report', { ok: false, code: 1, output: '' })
    s.revisions.set('review/sol/round-1', revisionB)
    const out = await review(s.flow())
    assert.equal(out.passed, false)
    assert.deepEqual(out.reviews[0], { reviewer: 'sol', verdict: 'unclear', session: 's-sol', revision: revisionB })
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
    const out = await review(s.flow())
    assert.deepEqual([out.passed, out.round, out.revision], [false, 2, 'revision unavailable'])
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
    const out = await review(s.flow())
    assert.equal(out.passed, false)
    assert.equal(out.revision, 'revision unavailable')
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
  it('keeps the 0.1.0 acceptance question byte for byte for a saved stop without revision evidence', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', attempts: '2' },
      () => 'done',
      (key) => !key.startsWith('printer/'),
      (key, question) => {
        assert.equal(key, 'printer/after-2')
        assert.equal(
          question,
          'unit printer failed its acceptance commands 2 times (test printer; lint). Fix it or narrow it, then retry; or skip it, or stop.',
        )
        return 'stop'
      },
      { 'u.json': spec },
    )
    const out = await units(s.flow())
    assert.equal(out.stopped, 'printer')
    assert.equal(out.revision, 'revision unavailable')
  })

  it('keeps the 0.1.0 fix prompt byte for byte when every failed command lacks a revision', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b' },
      () => 'done',
      (key) => !key.startsWith('printer/try-1/'),
      never,
      { 'u.json': spec },
    )
    assert.equal((await units(s.flow())).units[1]?.revision, 'revision unavailable')
    assert.equal(
      s.prompts.get('printer/fix-1'),
      [
        'The acceptance commands of unit printer failed. Fix your work, not the commands:',
        '',
        '$ test printer  (exit 1)',
        'test printer failed',
        '',
        '$ lint  (exit 1)',
        'lint failed',
        '',
        'End your reply with what you changed.',
      ].join('\n'),
    )
  })

  for (const [name, tries, finalKeys] of [
    ['review', 1, ['review/fix-1']],
    ['review', 2, ['review/final-fix-1']],
    ['review', 3, ['review/final-fix-1', 'review/final-fix-2', 'review/fix-3']],
    ['parser', 2, ['review/fix-1']],
  ] as const) {
    it(`uses ${finalKeys.join(', ')} for final review after ${name} passes on attempt ${tries}`, async () => {
      const one = JSON.stringify({ units: [{ name, task: 'Implement it.', accept: ['test feature'] }] })
      const rounds = finalKeys.length + 1
      const s = new Scripted(
        {
          units: 'u.json',
          implementer: 'pi',
          branch: 'b',
          reviewers: 'sol',
          attempts: String(tries),
          rounds: String(rounds),
        },
        (key) => (key === `review/sol/round-${rounds}` ? 'VERDICT: pass' : 'VERDICT: fail'),
        (key) => !key.startsWith(`${name}/try-`) || key === `${name}/try-${tries}/accept-1`,
        never,
        { 'u.json': one },
      )
      const out = await units(s.flow())
      assert.equal(out.units[0]?.tries, tries)
      assert.equal(out.review?.passed, true)
      assert.equal(out.review?.round, rounds)
      for (let n = 1; n < tries; n++) {
        assert.ok(s.prompts.get(`${name}/fix-${n}`)?.startsWith(`The acceptance commands of unit ${name} failed.`))
      }
      const fixes = [...s.prompts].filter(([, prompt]) => prompt.startsWith('Reviewers found issues in your work.'))
      assert.deepEqual(
        fixes.map(([key]) => key),
        finalKeys,
      )
    })
  }

  async function replaySavedStop(t: TestContext, fixture: string, stopKey: string, expectedSends: string[]) {
    const dir = mkdtempSync(join(tmpdir(), 'tmag-flow-replay-'))
    made.push(dir)
    // Both journals were captured with the flow/v0.1.0 CLI after its stop decision.
    copyFileSync(new URL(`../fixtures/${fixture}`, import.meta.url), join(dir, 'journal.jsonl'))
    const journal = Journal.open(dir)
    t.after(() => journal.close())
    const run = journal.run
    assert.ok(run)
    assert.equal(run.flow, '0.1.0')
    assert.equal(journal.done('review/fix-1')?.kind, 'ask')
    assert.equal(journal.halt(stopKey)?.decision?.choice, 'stop')
    const sends = journal.all('send')
    assert.deepEqual(
      sends.map((s) => s.key),
      expectedSends,
    )
    const tmag = new Tmag('/bin/false')
    tmag.run = async ([verb]) => {
      assert.equal(verb, 'inspect', 'replay only checks the bound implementer')
      return { code: 0, docs: [], stdout: '', stderr: '', error: undefined, message: undefined }
    }
    const rt = new Runtime({
      run: run.run,
      project: run.project,
      args: run.args,
      budget: run.budget,
      dir,
      journal,
      tmag,
      caller: null,
      deadline: undefined,
      startName: false,
      say: () => {},
    })
    const flow = rt.flow()
    const originalArtifacts = join(run.project, runsDirName, run.run, 'artifacts')
    const result = await units({
      ...flow,
      // The original report path is part of the saved ask and gate fingerprints.
      // The response is written under this test's temporary run directory.
      path: (...parts) => (parts[0] === 'reports' ? join(originalArtifacts, ...parts) : flow.path(...parts)),
    })
    assert.equal(journal.done(stopKey)?.result, 'stop')
    assert.deepEqual(journal.all('send'), sends)
    return result
  }

  it('replays a completed 0.1.0 unit fix and honors the saved stop', async (t) => {
    const out = await replaySavedStop(t, 'units-review-fix-0.1.0.jsonl', 'review/after-2', [
      'review/task',
      'review/fix-1',
    ])
    assert.deepEqual(out, { stopped: 'review', revision: 'revision unavailable', units: [] })
  })

  it('replays a completed 0.1.0 final review fix and honors the saved stop', async (t) => {
    const out = await replaySavedStop(t, 'units-final-review-fix-0.1.0.jsonl', 'review/accept-1/after-1', [
      'review/task',
      'review/sol/round-1',
      'review/fix-1',
    ])
    assert.deepEqual(out.units, [{ unit: 'review', result: 'passed', tries: 1, revision: 'revision unavailable' }])
    assert.equal(out.review?.passed, false)
    assert.equal(out.review?.round, 1)
  })

  it('completes a unit named review and its final review with distinct step keys', async () => {
    const one = JSON.stringify({ units: [{ name: 'review', task: 'Implement it.', accept: ['test feature'] }] })
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', reviewers: 'sol' },
      (key) => (key === 'review/sol/round-1' ? 'VERDICT: pass' : 'done'),
      () => true,
      never,
      { 'u.json': one },
    )
    const out = await units(s.flow())
    assert.equal(out.units[0]?.unit, 'review')
    assert.equal(out.review?.passed, true)
  })

  it('runs each unit through its acceptance commands, sending failures back', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'feat/x' },
      () => 'done',
      (key) => !key.startsWith('printer/try-1/'),
      never,
      { 'u.json': spec },
    )
    s.revisions.set('parser/try-1/accept-1', revisionA)
    s.revisions.set('printer/try-1/accept-1', revisionB)
    s.revisions.set('printer/try-2/accept-1', revisionA)
    s.revisions.set('printer/try-2/accept-2', revisionB)
    const out = await units(s.flow())
    assert.deepEqual(
      out.units.map((u) => [u.unit, u.tries, u.revision]),
      [
        ['parser', 1, revisionA],
        ['printer', 2, `mixed (${revisionA}; ${revisionB})`],
      ],
    )
    assert.deepEqual(
      s.steps.filter((step) => step.startsWith('gate ')),
      [
        'gate parser/try-1/accept-1',
        'gate printer/try-1/accept-1',
        'gate printer/try-1/accept-2',
        'gate printer/try-2/accept-1',
        'gate printer/try-2/accept-2',
      ],
    )
    assert.deepEqual(s.roles.get('impl'), { agent: 'pi', worktree: 'feat/x' })
    assert.ok(s.steps.includes('ask printer/fix-1 impl'))
    const fix = s.prompts.get('printer/fix-1') as string
    assert.ok(fix.startsWith('The acceptance commands of unit printer failed. Fix your work, not the commands:\n'))
    assert.ok(fix.includes(`$ test printer  (exit 1; ${revisionB})\ntest printer failed`))
    assert.ok(fix.includes('$ lint  (exit 1; revision unavailable)\nlint failed'))
    assert.match(s.prompts.get('printer/task') as string, /Unit 2 of 2: printer\.[\s\S]*\$ test printer\n {2}\$ lint/)
  })

  it('halts a unit that keeps failing, and skips it when told to', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', attempts: '2' },
      () => 'done',
      (_key, command) => command !== 'test parser',
      (key, question) => {
        assert.ok(question.includes(`The last attempt recorded ${revisionB}.`))
        return key === 'parser/after-2' ? 'skip' : never(key)
      },
      { 'u.json': spec },
    )
    s.revisions.set('parser/try-2/accept-1', revisionB)
    const out = await units(s.flow())
    assert.deepEqual(
      out.units.map((u) => [u.unit, u.result, u.revision]),
      [
        ['parser', 'skipped', revisionB],
        ['printer', 'passed', 'revision unavailable'],
      ],
    )
  })

  it('returns the stopped unit and its last revision alongside the units that finished', async () => {
    const s = new Scripted(
      { units: 'u.json', implementer: 'pi', branch: 'b', attempts: '2' },
      () => 'done',
      (_key, command) => command !== 'test printer',
      (key, question) => {
        assert.ok(question.includes(`The last attempt recorded mixed (${revisionB}; ${revisionA}).`))
        return key === 'printer/after-2' ? 'stop' : never(key)
      },
      { 'u.json': spec },
    )
    s.revisions.set('parser/try-1/accept-1', revisionA)
    s.revisions.set('printer/try-2/accept-1', revisionB)
    s.revisions.set('printer/try-2/accept-2', revisionA)
    assert.deepEqual(await units(s.flow()), {
      stopped: 'printer',
      revision: `mixed (${revisionB}; ${revisionA})`,
      units: [{ unit: 'parser', result: 'passed', tries: 1, revision: revisionA }],
    })
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
    s.revisions.set('review/sol/round-1', revisionA)
    s.revisions.set('review/astra/round-1', revisionA)
    s.revisions.set('review/astra/round-2', revisionB)
    const out = (await units(s.flow())) as { review: { passed: boolean; round: number; revision: string } }
    assert.deepEqual(out.review.passed, true)
    assert.equal(out.review.round, 2)
    assert.equal(out.review.revision, `mixed (${revisionA}; ${revisionB})`)
    for (const prompt of s.prompts.values()) assert.doesNotMatch(prompt, /The work is (?:now )?at /)
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
    s.revisions.set('review/sol/round-1', revisionA)
    s.revisions.set('review/accept-1/try-1/accept-1', revisionB)
    const out = (await units(s.flow())) as { review: { passed: boolean; revision: string } }
    assert.equal(out.review.passed, false)
    assert.equal(out.review.revision, revisionA)
    const fix = s.prompts.get('review/accept-1/fix-1') as string
    assert.ok(
      fix.includes('The acceptance commands of the fixes of review round 1 failed. Fix your work, not the commands:'),
    )
    assert.ok(fix.includes(`$ test one  (exit 1; ${revisionB})`))
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
