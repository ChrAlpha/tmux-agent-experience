import type { Computer } from '../computer'
import { minimum } from '../contract'
import { computers, later, linked, named, notice, route, title } from '../store'
import { Agents } from './Agents'
import { explain } from './explain'
import { Pair } from './Pair'
import { PaneScreen } from './Pane'
import { Session } from './Session'
import { Start } from './Start'

export function App() {
  const r = route.value
  const all = computers.value
  let screen = <Agents />
  if (!all.length) screen = <Pair />
  else if (r.name === 'add') screen = <Pair adding />
  else if (r.name === 'start') screen = <Start key={`start/${r.at}`} at={r.at} />
  else if (r.name === 'session' || r.name === 'pane') {
    const c = named(r.at)
    screen = !c ? (
      <Missing name={r.at} />
    ) : r.name === 'session' ? (
      <Session key={`${r.at}/${r.id}`} c={c} id={r.id} request={r.request} />
    ) : (
      <PaneScreen key={`${r.at}/${r.pane}`} c={c} pane={r.pane} />
    )
  }
  return (
    <div class="app">
      {all.map((c) => (
        <Bar key={c.name} c={c} several={all.length > 1} />
      ))}
      {all.length > 0 && notice.value && <Notice />}
      {later.value && <Later />}
      {screen}
    </div>
  )
}

// Bar says that a computer's watch does not deliver, and why, or that tmag
// on it is too old for the page. With several computers it names which.
function Bar({ c, several }: { c: Computer; several: boolean }) {
  const on = several ? `${title(c)}: ` : ''
  if (c.live.value && c.outdated.value) {
    return (
      <div class="bar error" role="status">
        {on}tmag {c.outdated.value} on the computer is older than this page needs. Update it to {minimum} or later.
      </div>
    )
  }
  if (c.live.value) return null
  const p = c.problem.value
  return (
    <div class={p ? 'bar error' : 'bar'} role="status">
      {on}
      {p ? `${explain(p)} Trying again…` : 'Connecting…'}
    </div>
  )
}

// Notice says why the page forgot a computer while it works with others.
function Notice() {
  return (
    <div class="bar error" role="alert">
      {notice.value} <a href="#/add">Pair it again</a>{' '}
      <button class="link" type="button" onClick={() => (notice.value = '')}>
        Dismiss
      </button>
    </div>
  )
}

// Later says what became of a start the person left before it ended.
function Later() {
  const l = later.value
  if (!l) return null
  return (
    <div class="bar" role="status">
      {l.text}{' '}
      {l.href && (
        <a href={l.href} onClick={() => (later.value = null)}>
          {l.label}
        </a>
      )}{' '}
      <button class="link" type="button" onClick={() => (later.value = null)}>
        Dismiss
      </button>
    </div>
  )
}

// Missing is a link to a computer the page does not work with.
function Missing({ name }: { name: string }) {
  return (
    <div class="screen session">
      <header>
        <a class="back" href="#/" aria-label="Agents">
          ‹
        </a>
        <div class="what">
          <h1 class="name">{name}</h1>
        </div>
      </header>
      <main class="scroll">
        <p class="empty">
          This page does not work with a computer named {name}.{' '}
          <button
            class="link"
            type="button"
            onClick={() => {
              linked.value = { hash: location.hash, name }
              location.hash = '#/add'
            }}
          >
            Add it
          </button>{' '}
          to open what it links to.
        </p>
      </main>
    </div>
  )
}
