import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, describe, it } from 'node:test'
import { main } from '../../src/main.ts'

// scratch is a directory of a test's own, gone once the file's tests end.
const made: string[] = []
function scratch(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  made.push(dir)
  return dir
}
after(() => {
  for (const dir of made) rmSync(dir, { recursive: true, force: true })
})

async function run(...argv: string[]) {
  let out = ''
  let err = ''
  const cwd = scratch('tmag-flow-cli-')
  execFileSync('git', ['init', '-q', cwd])
  const code = await main(argv, { out: (t) => (out += t), err: (t) => (err += t), cwd })
  return { code, out, err, cwd }
}

describe('the command line', () => {
  it('says what it takes, and exits 2 for anything else', async () => {
    assert.match((await run('--help')).out, /tmag-flow start {2}<plan>/)
    for (const argv of [['bogus'], ['start'], ['start', 'review', '--bogus'], ['resume'], ['decide', 'a', 'b']]) {
      const r = await run(...argv)
      assert.equal(r.code, 2, `${argv.join(' ')}: ${r.err}`)
      assert.match(r.err, /\nnext: /, argv.join(' '))
    }
    const bad = await run('start', 'review', '--arg', 'no-equals')
    assert.equal(bad.code, 2)
    assert.match(bad.err, /--arg takes KEY=VALUE/)
    assert.match((await run('start', 'review', '--for', '5')).err, /--for is a number and a unit/)
    assert.match((await run('start', 'nope')).err, /no plan nope: the plans that ship are review, units/)
  })

  it('names its version and its plans', async () => {
    assert.match((await run('version')).out, /^tmag-flow \d+\.\d+\.\d+ \(tmag-flow\/v1\)\n$/)
    const plans = JSON.parse((await run('plans', '--json')).out) as { plans: { name: string }[] }
    assert.deepEqual(
      plans.plans.map((p) => p.name),
      ['review', 'units'],
    )
  })

  it('finds no run where none ran', async () => {
    const r = await run('status')
    assert.equal(r.code, 1)
    assert.match(r.err, /no runs in .*\nnext: start one with tmag-flow start PLAN/)
    assert.match((await run('list')).out, /^no runs in /)
    assert.equal((await run('resume', 'r1')).code, 2)
  })

  it('needs tmag 0.8.0 or later', async () => {
    const dir = scratch('tmag-flow-old-')
    const old = join(dir, 'tmag')
    execFileSync('sh', ['-c', `printf '#!/bin/sh\\necho "tmag 0.7.1 (tmag/v2)"\\n' > ${old}; chmod +x ${old}`])
    const before = process.env.TMAG_FLOW_TMAG
    process.env.TMAG_FLOW_TMAG = old
    try {
      const r = await run('start', 'review')
      assert.equal(r.code, 1)
      assert.match(r.err, /needs tmag 0\.8\.0 or later, and .* is 0\.7\.1\nnext: update the tmux plugin/)
    } finally {
      if (before === undefined) delete process.env.TMAG_FLOW_TMAG
      else process.env.TMAG_FLOW_TMAG = before
    }
  })
})
