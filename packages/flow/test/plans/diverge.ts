import { readFileSync } from 'node:fs'
import type { Flow } from '../../src/index.ts'

// An ask whose prompt the test rewrites between invocations, then a decision.
export default async function diverge(flow: Flow) {
  const impl = flow.role('impl', { agent: 'impl' })
  const prompt = readFileSync(flow.args.prompt as string, 'utf8')
  const a = await flow.ask('impl-1', impl, prompt)
  const choice = await flow.decide('go-on', 'go on?', ['go', 'stop'])
  return { text: a.text, choice }
}
