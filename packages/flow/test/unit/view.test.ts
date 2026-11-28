import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, it, type TestContext } from 'node:test'
import { Journal, type StepKind } from '../../src/journal.ts'
import { render, viewOf } from '../../src/view.ts'

function statusOf(t: TestContext, ...steps: [key: string, kind: StepKind, result: unknown][]) {
  const dir = mkdtempSync(join(tmpdir(), 'tmag-flow-view-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const journal = Journal.open(dir)
  journal.append({
    t: 'run',
    layout: 'tmag-flow/journal/v1',
    run: 'r',
    plan: 'checks',
    planPath: 'checks.ts',
    planHash: 'hash',
    args: {},
    project: dir,
    flow: '0.1.0',
    budget: { concurrency: 1, agents: 1, askTimeout: 60_000 },
  })
  for (const [key, kind, result] of steps) {
    journal.append({ t: 'intent', key, kind, fp: key, attempt: 1 })
    journal.append({ t: 'done', key, kind, fp: key, result })
  }
  journal.close()
  return viewOf(Journal.open(dir), dir)
}

describe('status', () => {
  it('shows the verdict and last non-empty output line of each gate on one row', (t) => {
    const passed = 'running suites\nran: 3/3 suites\n\n'
    const failed = 'building\nmissing target\n'
    const view = statusOf(
      t,
      ['suites', 'gate', { ok: true, code: 0, output: passed }],
      ['build', 'gate', { ok: false, code: 1, output: failed }],
    )
    assert.deepEqual(
      view.steps.map(({ summary, code, output }) => [summary, code, output]),
      [
        ['passed: ran: 3/3 suites', 0, passed],
        ['failed (exit 1): missing target', 1, failed],
      ],
    )
    const rows = render(view).split('steps:\n')[1]?.split('\n')
    assert.deepEqual(
      rows?.map((row) => row.split(/ {2,}/)),
      [
        ['STEP', 'KIND', 'STATE', 'SUMMARY'],
        ['suites', 'gate', 'done', 'passed: ran: 3/3 suites'],
        ['build', 'gate', 'done', 'failed (exit 1): missing target'],
      ],
    )
  })

  it('caps an exec summary at 80 characters of its last line and keeps the last 4096 characters of output', (t) => {
    const prefix = 'a'.repeat(77)
    const output = `building\n${prefix}${'b'.repeat(4096)}\n`
    const [step] = statusOf(t, ['build', 'exec', { ok: true, code: 0, output }]).steps
    assert.equal(step?.summary, `passed: ${prefix}...`)
    assert.equal(step?.code, 0)
    assert.equal(step?.output, `${'b'.repeat(4095)}\n`)
  })

  it('shows the verdict alone for commands with empty output', (t) => {
    const view = statusOf(
      t,
      ['quiet', 'gate', { ok: true, code: 0, output: '' }],
      ['error', 'exec', { ok: false, code: 7, output: '' }],
    )
    assert.deepEqual(
      view.steps.map(({ summary, code, output }) => [summary, code, output]),
      [
        ['passed', 0, ''],
        ['failed (exit 7)', 7, ''],
      ],
    )
  })

  it('omits command fields for other kinds and for an abandoned command', (t) => {
    const view = statusOf(
      t,
      ['review', 'ask', { turn: '1', text: 'done', session: 's' }],
      ['choice', 'decide', 'stop'],
      ['spec', 'freeze', 'text'],
      ['cancelled', 'exec', { abandoned: 'decided' }],
    )
    assert.equal(view.steps.at(-1)?.state, 'abandoned')
    for (const step of view.steps) {
      assert.equal('code' in step, false, step.key)
      assert.equal('output' in step, false, step.key)
    }
  })
})
