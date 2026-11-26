import type { Flow } from '../../src/index.ts'

// A plan that exercises every primitive against fake agents, which answer
// each prompt with "answered: " and its last line.
export default async function basic(flow: Flow) {
  const impl = flow.role('impl', { agent: flow.args.impl ?? 'impl', worktree: flow.args.branch ?? 'unit-1' })
  const first = await flow.ask('impl-1', impl, 'Implement unit 1.\nunit 1 done')
  const where = await flow.bind(impl)
  const gate = await flow.gate('gate-1', flow.args.gate ?? 'test -f ok', { cwd: where.dir })
  let choice = 'none'
  if (!gate.ok) choice = await flow.decide('gate-1-failed', 'the gate failed: retry or stop?', ['retry', 'stop'])
  return { first: first.text, gate: gate.ok, choice, dir: where.dir }
}
