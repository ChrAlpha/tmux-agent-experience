import type { Flow } from '../../src/index.ts'

// Two roles asked at once.
export default async function pair(flow: Flow) {
  const sol = flow.role('sol', { agent: 'sol' })
  const astra = flow.role('astra', { agent: 'astra' })
  const [a, b] = await Promise.all([
    flow.ask('sol-1', sol, 'Review.\nsol verdict'),
    flow.ask('astra-1', astra, 'Review.\nastra verdict'),
  ])
  return { sol: a.text, astra: b.text, report: flow.path('reports', 'r1.md') }
}
