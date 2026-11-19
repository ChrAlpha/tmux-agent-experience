import type { Computer } from '../computer'
import { minimum } from '../contract'
import { hash } from '../route'
import { computers, holding, later, linked, named, notice, route, title } from '../store'
import { explain } from './explain'
import { useFrame } from './frame'
import { Header } from './Header'
import { Home } from './Home'
import { Pair } from './Pair'
import { PaneScreen } from './Pane'
import { Session } from './Session'
import { Settings } from './Settings'
import { Start } from './Start'

export function App() {
  const r = route.value
  const all = computers.value
  let screen = <Home />
  if (!all.length) screen = <Pair />
  else if (r.name === 'add') screen = <Pair adding />
  else if (r.name === 'settings') screen = <Settings />
  else if (r.name === 'start' && (named(r.at) || !(r.at || r.dir)))
    // The sheet tells one start link from another itself.
    screen = <Start key="start" at={r.at} dir={r.dir} />
  else if (r.name === 'start' || r.name === 'session' || r.name === 'pane') {
    const c = named(r.at)
    screen =
      !c || r.name === 'start' ? (
        <Missing name={r.at} />
      ) : c.info.value && !c.info.value.instance ? (
        <Nameless c={c} />
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
// One that is still connecting says so at the computers.
function Bar({ c, several }: { c: Computer; several: boolean }) {
  const on = several ? `${title(c)}: ` : ''
  if (c.live.value && c.outdated.value) {
    return (
      <div class="bar error" role="status">
        {on}tmag {c.outdated.value} on the computer is older than this page needs. Update it to {minimum} or later.
      </div>
    )
  }
  const p = c.problem.value
  if (c.live.value || !p) return null
  return (
    <div class="bar error" role="status">
      {on}
      {explain(p)} Trying again…
    </div>
  )
}

// Notice says why the page forgot a computer while it works with others.
function Notice() {
  return (
    <div class="bar error" role="alert">
      {notice.value} <a href={hash({ name: 'add' })}>Pair it again</a>{' '}
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
  const on = l.go && 'at' in l.go && l.of ? holding(l.instance, l.of) : undefined
  const href = l.go && 'at' in l.go && on ? hash({ ...l.go, at: on.name }) : undefined
  return (
    <div class="bar" role="status">
      {l.text}{' '}
      {href && (
        <a href={href} onClick={() => (later.value = null)}>
          {l.label}
        </a>
      )}{' '}
      <button class="link" type="button" onClick={() => (later.value = null)}>
        Dismiss
      </button>
    </div>
  )
}

// Nameless is a link to a computer that does not say which computer it is,
// of which the page shows nothing.
function Nameless({ c }: { c: Computer }) {
  const frame = useFrame()
  return (
    <div class="screen session" ref={frame}>
      <Header title={title(c)} />
      <main class="scroll">
        <p class="empty">{c.problem.value ? explain(c.problem.value) : 'Asking the computer which it is…'}</p>
      </main>
    </div>
  )
}

// Missing is a link to a computer the page does not work with.
function Missing({ name }: { name: string }) {
  const which = name ? `a computer named ${name}` : `the computer at ${location.host}`
  const frame = useFrame()
  return (
    <div class="screen session" ref={frame}>
      <Header title={name || location.host} />
      <main class="scroll">
        <p class="empty">
          This page does not work with {which}.{' '}
          <button
            class="link"
            type="button"
            onClick={() => {
              linked.value = { hash: location.hash, name }
              location.hash = hash({ name: 'add' })
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
