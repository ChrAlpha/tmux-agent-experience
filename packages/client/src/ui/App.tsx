import { minimum } from '../contract'
import { credential, live, outdated, problem, route } from '../store'
import { Agents } from './Agents'
import { explain } from './explain'
import { Pair } from './Pair'
import { Session } from './Session'

export function App() {
  const r = route.value
  return (
    <div class="app">
      <Bar />
      {!credential.value ? <Pair /> : r.name === 'session' ? <Session key={r.id} id={r.id} /> : <Agents />}
    </div>
  )
}

// Bar says, while the page is paired, that the watch does not deliver, and
// why, or that tmag on the computer is too old for the page.
function Bar() {
  if (!credential.value) return null
  if (live.value && outdated.value) {
    return (
      <div class="bar error" role="status">
        tmag {outdated.value} on the computer is older than this page needs. Update it to {minimum} or later.
      </div>
    )
  }
  if (live.value) return null
  const p = problem.value
  return (
    <div class={p ? 'bar error' : 'bar'} role="status">
      {p ? `${explain(p)} Trying again…` : 'Connecting…'}
    </div>
  )
}
