import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, it } from 'node:test'
import { code, main } from '../../src/main.ts'

const root = join(import.meta.dirname, '../..')
const read = (path: string) => readFileSync(join(root, path), 'utf8')
const skill = read('skills/flow/SKILL.md')
// What an agent runs as it is written: code blocks and inline code.
const runnable = [...skill.matchAll(/```[a-z]*\n([\s\S]*?)```|`([^`\n]+)`/g)].map((m) => m[1] ?? m[2]).join('\n')

async function usage(): Promise<string> {
  let out = ''
  await main(['--help'], { out: (t) => (out += t), err: () => {}, cwd: root })
  return out
}

describe('the skill', () => {
  it('names only the commands and flags tmag-flow has', async () => {
    const text = await usage()
    const commands = new Set([...text.matchAll(/^ {2}tmag-flow (\w+)/gm)].map((m) => m[1]))
    const named = [...runnable.matchAll(/tmag-flow (\w+)/g)].map((m) => m[1])
    assert.ok(named.length > 5)
    for (const cmd of named) assert.ok(commands.has(cmd), `the skill runs tmag-flow ${cmd}`)
    const flags = new Set([...text.matchAll(/--[a-z-]+/g)].map((m) => m[0]))
    for (const [flag] of runnable.matchAll(/--[a-z-]+/g)) assert.ok(flags.has(flag), `the skill names ${flag}`)
  })

  it('names the exit codes as tmag-flow has them', () => {
    for (const [, n] of skill.matchAll(/\*\*(\d)\*\*/g)) {
      assert.ok(Object.values(code).includes(Number(n) as never), `exit ${n}`)
    }
    assert.match(skill, new RegExp(`\\*\\*${code.halted}\\*\\*: a step halted`))
    assert.match(skill, new RegExp(`\\*\\*${code.stopped}\\*\\*: the run spent its`))
  })

  it('names only the arguments the plans that ship take', () => {
    for (const plan of ['review', 'units']) {
      const section = skill.slice(skill.indexOf(`- \`${plan}\``)).split('\n- ')[0] as string
      const doc = read(`plans/${plan}.ts`)
      for (const [, name] of section.matchAll(/--arg (\w+)/g)) {
        assert.match(doc, new RegExp(`//\\s+--arg ${name}\\b`), `${plan} takes --arg ${name}`)
      }
    }
  })

  it('names only what a flow has', () => {
    const types = read('src/types.ts')
    for (const [, m] of runnable.matchAll(/flow\.(\w+)\(/g))
      assert.match(types, new RegExp(`^ {2}${m}\\(`, 'm'), `flow.${m}`)
  })

  it('is the same for every host', () => {
    for (const copy of [
      'plugins/claude/tmag-flow/skills/flow/SKILL.md',
      'plugins/codex/tmag-flow/skills/flow/SKILL.md',
    ]) {
      assert.equal(read(copy), skill, `${copy} is a copy of skills/flow/SKILL.md`)
    }
  })
})

describe('versions', () => {
  it('are the same in the package and every plugin', () => {
    const version = (JSON.parse(read('package.json')) as { version: string }).version
    for (const manifest of [
      'plugins/claude/tmag-flow/.claude-plugin/plugin.json',
      'plugins/codex/tmag-flow/.codex-plugin/plugin.json',
    ]) {
      assert.equal((JSON.parse(read(manifest)) as { version: string }).version, version, manifest)
    }
  })
})
