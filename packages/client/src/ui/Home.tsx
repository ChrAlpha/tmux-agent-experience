import type { ComponentChildren } from 'preact'
import { useLayoutEffect, useRef } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Place } from '../contract'
import { apart, tilde, where } from '../places'
import { hash, type Route } from '../route'
import { computers, type Entry, go, home, needs, projects, sections, title, waiting } from '../store'
import { lit, Navigation, Surface, Tile } from './axiom'
import { cls } from './explain'
import { useFrame } from './frame'
import { Round } from './Header'
import { Branch, Computers, Plus } from './icons'
import { Row } from './Row'

// Home is every computer's sessions, as an inbox of what waits on the
// person, works and rests, or by the projects they run in; with the way to
// start another agent and to the computers this device works with.
// Where the person scrolled each view to, for as long as the page is open.
const scrolled: Record<string, number> = {}

// A view the person switches to comes in from the side, as a pivot's does.
const still = typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)')

export function Home() {
  const view = home.value
  const frame = useFrame()
  // Coming back to a view, the person finds it where they left it.
  const main = useRef<HTMLElement>(null)
  const shown = useRef(view)
  // biome-ignore lint/correctness/useExhaustiveDependencies: a view the person switches to is scrolled where they left it
  useLayoutEffect(() => {
    const el = main.current
    if (!el) return
    el.scrollTop = scrolled[view] ?? 0
    if (shown.current === view) return
    shown.current = view
    if (!still || still.matches) return
    el.animate(
      [
        { opacity: 0, transform: 'translateX(40px)' },
        { opacity: 1, transform: 'none' },
      ],
      {
        duration: 360,
        easing: 'cubic-bezier(0.1, 0.9, 0.2, 1)',
      },
    )
  }, [view])
  const all = computers.value
  const loaded = all.every((c) => c.loaded.value)
  // Until each computer answers or says why not, the sessions are still to come.
  const coming = all.some((c) => !c.loaded.value && !c.problem.value)
  const known = all.some((c) => c.view.value.sessions.size)
  const starts = all.some((c) => c.can('start'))
  const health = all.some((c) => c.problem.value || c.outdated.value) ? 'bad' : loaded ? '' : 'wait'
  return (
    <div class="screen home" ref={frame}>
      <Surface class="bar-top" layers="acrylic">
        <header class="head">
          <div class="head-row">
            <span class="app-name">tmag</span>
            <div class="actions-top">
              {starts && (
                <Round href={hash({ name: 'start', at: '' })} label="New agent">
                  <Plus />
                </Round>
              )}
              <Round href={hash({ name: 'settings' })} class={cls('health', health)} label="Computers">
                <Computers />
              </Round>
            </div>
          </div>
          <h1 class="hidden">{view === 'inbox' ? 'Inbox' : 'Projects'}</h1>
          <nav aria-label="Sessions">
            <Navigation at={view === 'inbox' ? 0 : 1}>
              <ax-navigation-item default={view === 'inbox'}>
                <Tab to="inbox" current={view}>
                  Inbox{waiting.value > 0 && <span class="badge">{waiting.value}</span>}
                </Tab>
              </ax-navigation-item>
              <ax-navigation-item default={view === 'projects'}>
                <Tab to="projects" current={view}>
                  Projects
                </Tab>
              </ax-navigation-item>
            </Navigation>
          </nav>
        </header>
      </Surface>
      <main
        class="scroll"
        ref={main}
        onScroll={(e) => {
          scrolled[view] = e.currentTarget.scrollTop
        }}
      >
        {!known && coming ? (
          <Skeleton />
        ) : !known && !loaded ? (
          <div class="empty">
            <p>No computer answers.</p>
            <p class="muted">The bar above says why. The page keeps trying.</p>
          </div>
        ) : view === 'inbox' ? (
          <Inbox starts={starts} />
        ) : (
          <Projects starts={starts} />
        )}
      </main>
    </div>
  )
}

function Tab({ to, current, children }: { to: 'inbox' | 'projects'; current: string; children: ComponentChildren }) {
  return (
    <a
      class="pivot"
      href={hash({ name: to })}
      aria-current={to === current ? 'page' : undefined}
      onClick={(e) => {
        // Switching views takes no entry of its own: back leaves the sessions.
        e.preventDefault()
        go({ name: to }, { replace: true })
      }}
    >
      {children}
    </a>
  )
}

function Skeleton() {
  return (
    <ul class="rows" aria-busy="true" aria-label="Reading sessions">
      {[0, 1, 2].map((i) => (
        <li key={i} class="row skeleton">
          <span class="mark" />
          <span class="body">
            <span class="bone" />
            <span class="bone short" />
          </span>
        </li>
      ))}
    </ul>
  )
}

function Section({ label, entries }: { label: string; entries: Entry[] }) {
  if (!entries.length) return null
  return (
    <section class="section" aria-label={label}>
      <h2 class="label">
        {label} <span class="n">{entries.length}</span>
      </h2>
      <ul class="rows">
        {entries.map((e) => (
          <Row key={`${e.c.name}/${e.s.agent}/${e.s.id}`} e={e} />
        ))}
      </ul>
    </section>
  )
}

function Empty({ starts }: { starts: boolean }) {
  return (
    <div class="empty">
      <p>No agent is running.</p>
      <p class="muted">Start one here or in tmux, and it shows up as soon as tmag sees it.</p>
      {starts && (
        <Tile class="tile wide" layers={lit('fill')}>
          <span class="face">
            <Plus />
          </span>
          <a slot="action" href={hash({ name: 'start', at: '' })}>
            <span class="name">New agent</span>
          </a>
        </Tile>
      )}
    </div>
  )
}

function Inbox({ starts }: { starts: boolean }) {
  const { waits, works, rest } = sections.value
  if (!waits.length && !works.length && !rest.length) return <Empty starts={starts} />
  return (
    <>
      {!waits.length && <p class="clear">Nothing waits on you.</p>}
      <Section label="Needs you" entries={waits} />
      <Section label="Working" entries={works} />
      <Section label="Idle" entries={rest} />
    </>
  )
}

// Projects are the sessions by the project they run in, each with the way
// to start another agent there; then the places sessions ran in lately that
// none runs in now.
function Projects({ starts }: { starts: boolean }) {
  const all = projects.value
  const several = computers.value.length > 1
  const busy = new Set(all.map((p) => `${p.c.name}\u0000${p.where.root}`))
  const recent = computers.value.flatMap((c) => idle(c, busy))
  // A tile shows its path, kept to its end; tiles that name alike show what
  // tells theirs apart, filling two lines (see apart);
  // over it, its computer, where the page works with several, and under
  // it, its branch: four lines, which the smallest tile holds.
  const paths = recent.map(({ c, place }) => tilde(place.dir, c.places.value.value?.home))
  const named = new Map<string, number[]>()
  for (const [i, { w }] of recent.entries()) named.set(w.name, [...(named.get(w.name) ?? []), i])
  const ends = new Map<number, string>()
  for (const same of named.values()) {
    if (same.length < 2) continue
    const told = apart(same.map((i) => paths[i] ?? ''))
    for (const [n, i] of same.entries()) ends.set(i, told[n] ?? '')
  }
  if (!all.length && !recent.length) return <Empty starts={starts} />
  return (
    <>
      {all.map((p) => {
        const waits = p.entries.filter(needs).length
        return (
          <section key={p.key} class="project" aria-label={p.where.name}>
            <div class="project-head">
              <div class="what">
                <h2 class="name">
                  {p.where.name}
                  {waits > 0 && <span class="badge">{waits}</span>}
                </h2>
                <div class="meta">
                  {[tilde(p.where.root, p.c.places.value.value?.home), several && title(p.c)]
                    .filter(Boolean)
                    .join(' · ')}
                </div>
              </div>
              {starts && p.c.can('start') && p.where.root.startsWith('/') && (
                <StartHere to={{ name: 'start', at: p.c.name, dir: p.where.root }} name={p.where.name} />
              )}
            </div>
            <ul class="rows">
              {p.entries.map((e) => (
                <Row key={`${e.c.name}/${e.s.agent}/${e.s.id}`} e={e} in />
              ))}
            </ul>
          </section>
        )
      })}
      {recent.length > 0 && (
        <section class="section" aria-label="Recent">
          <h2 class="label">Recent</h2>
          <ul class="tiles">
            {recent.map(({ c, place, w }, i) => {
              const path = paths[i] ?? place.dir
              const end = ends.get(i)
              return (
                <li key={`${c.name}/${place.dir}`}>
                  <Tile class="tile" layers={lit('fill')}>
                    <span class="face">
                      {several && <span>{title(c)}</span>}
                      {end ? (
                        <span class="at apart">{end}</span>
                      ) : (
                        <span class="at">
                          <bdi>{path}</bdi>
                        </span>
                      )}
                      {w.branch && (
                        <span class="chip">
                          <Branch />
                          {w.branch}
                        </span>
                      )}
                    </span>
                    <a
                      slot="action"
                      href={hash({ name: 'start', at: c.name, dir: place.dir })}
                      aria-label={`${w.name}: new agent in ${path}${w.branch ? `, ${w.branch}` : ''}${several ? ` on ${title(c)}` : ''}`}
                    >
                      <span class="name">{w.name}</span>
                    </a>
                  </Tile>
                </li>
              )
            })}
          </ul>
        </section>
      )}
    </>
  )
}

// idle is the places a computer's sessions ran in lately that no project
// with sessions holds now, the latest of each of a project's worktrees.
function idle(c: Computer, busy: Set<string>): { c: Computer; place: Place; w: ReturnType<typeof where> }[] {
  if (!c.can('start')) return []
  const seen = new Set<string>()
  return (c.places.value.value?.recent ?? []).flatMap((place) => {
    const w = where(place.dir, place.repo)
    const key = place.repo?.top ?? place.dir
    if (busy.has(`${c.name}\u0000${w.root}`) || seen.has(key)) return []
    seen.add(key)
    return [{ c, place, w }]
  })
}

function StartHere({ to, name }: { to: Route; name: string }) {
  return (
    <Round href={hash(to)} label={`New agent in ${name}`}>
      <Plus />
    </Round>
  )
}
