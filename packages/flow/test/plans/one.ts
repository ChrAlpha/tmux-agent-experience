import type { Flow } from '../../src/index.ts'

// One ask of one role.
export default async function one(flow: Flow) {
  const impl = flow.role('impl', { agent: flow.args.agent ?? 'impl' })
  const a = await flow.ask('impl-1', impl, `Work on it.\n${flow.args.line ?? 'one done'}`)
  return a.text
}
