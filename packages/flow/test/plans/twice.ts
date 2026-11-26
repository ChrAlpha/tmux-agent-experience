import type { Flow } from '../../src/index.ts'

// Two asks of one role, with a gate between them that waits for a file the
// test writes, so the test can end the role's session in between.
export default async function twice(flow: Flow) {
  const impl = flow.role('impl', { agent: 'impl' })
  const a = await flow.ask('impl-1', impl, 'First.\nfirst done')
  await flow.gate('go', `while [ ! -f ${flow.args.go} ]; do sleep 0.1; done`, { timeout: '60s' })
  const b = await flow.ask('impl-2', impl, 'Second.\nsecond done')
  return [a.text, b.text]
}
