import { useEffect, useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Session as S } from '../contract'
import { hash } from '../route'
import { computers, go, nextAfter, now, sections, title, whereIs } from '../store'
import { agentName, ago, find, helpers, paneOf, shared, status, ties } from '../view'
import { Navigation, Surface } from './axiom'
import { Changes } from './Changes'
import { Composer } from './Composer'
import { cls } from './explain'
import { useFrame } from './frame'
import { Header, Round } from './Header'
import { Branch, Next, Plus } from './icons'
import { KeyBar } from './KeyBar'
import { Requests } from './Requests'
import { Screen } from './Screen'
import { Scroller } from './Scroller'
import { Turns } from './Turns'
import { useToast } from './toast'

type Mode = 'turns' | 'screen' | 'changes'
const named: Record<Mode, string> = { turns: 'Turns', screen: 'Screen', changes: 'Changes' }

// Session is one session of a computer: where it runs and what it does, its
// turns, the screen of the pane that shows it and what its directory
// changed, where its computer serves each, what it asks, and the box to
// prompt it. From there the person goes on to the next session that waits
// on them, or starts another agent where it runs.
export function Session({ c, id, request }: { c: Computer; id: string; request?: string }) {
  const v = c.view.value
  const ambiguous = shared(v, id)
  const s = ambiguous ? undefined : find(v, id)
  const pane = s && c.can('read') ? paneOf(v, s) : undefined
  const modes: Mode[] = [
    'turns',
    ...(pane ? ['screen' as const] : []),
    ...(s && c.can('changes') ? ['changes' as const] : []),
  ]
  const [chosen, setMode] = useState<Mode>('turns')
  // A request a link names shows with the turns, where its card is open.
  useEffect(() => {
    if (request) setMode('turns')
  }, [request])
  const mode = modes.includes(chosen) ? chosen : 'turns'
  const showScreen = mode === 'screen'
  const gone = c.loaded.value && !s
  const [toast, say] = useToast()
  const frame = useFrame()

  // What the session shows is read again whenever the watch reports it: the
  // view makes a new object of each session an event or a snapshot reports.
  useEffect(() => {
    if (!s) return
    if (mode === 'screen') c.loadScreen(id)
    else if (mode === 'changes') c.loadChanges(id)
    else if (c.can('read')) c.loadTurns(id)
  }, [c, id, s, mode])
  // A screen also changes with nothing to report, as an agent writes: it is
  // read again every two seconds while it shows.
  useEffect(() => {
    if (!showScreen) return
    const again = setInterval(() => document.hidden || c.loadScreen(id), 2000)
    return () => clearInterval(again)
  }, [c, id, showScreen])
  // The person sees the turn it ended, and one that ends while they look,
  // while the page shows: a turn that ends while it is hidden is seen once
  // it shows again.
  const seen = c.seen.value
  useEffect(() => {
    if (!s || !seen) return
    const look = () => document.hidden || c.looked(id)
    look()
    document.addEventListener('visibilitychange', look)
    return () => document.removeEventListener('visibilitychange', look)
  }, [c, id, s, seen])

  const w = s && whereIs({ c, s })
  const place = w && [w.branch, w.sub].filter(Boolean).join(' · ')
  return (
    <div class="screen session" ref={frame}>
      <Header
        title={
          <>
            {w?.name ?? (s ? s.id.slice(0, 8) : 'Session')}
            {place && (
              <span class={cls('chip', w?.worktree && 'tree')}>
                {w?.branch && <Branch />}
                {place}
              </span>
            )}
          </>
        }
        meta={
          s
            ? [
                status(s),
                ago(s.since, now.value),
                agentName(s.agent),
                helpers(s),
                computers.value.length > 1 && title(c),
              ]
                .filter(Boolean)
                .join(' · ')
            : gone
              ? 'not running'
              : ''
        }
        actions={
          <>
            <Onward c={c} id={id} />
            {s?.cwd && c.can('start') && (
              <Round href={hash({ name: 'start', at: c.name, dir: s.cwd })} label="New agent here">
                <Plus />
              </Round>
            )}
          </>
        }
        below={
          modes.length > 1 &&
          !gone &&
          !ambiguous && (
            <nav class="tabs" aria-label="Show">
              <Navigation at={modes.indexOf(mode)}>
                {modes.map((m) => (
                  <ax-navigation-item key={m} default={m === mode}>
                    <button class="tab" type="button" aria-pressed={m === mode} onClick={() => setMode(m)}>
                      {named[m]}
                    </button>
                  </ax-navigation-item>
                ))}
              </Navigation>
            </nav>
          )
        }
      >
        {s && <Ties c={c} s={s} />}
      </Header>
      <Scroller key={mode}>
        {ambiguous ? (
          <p class="empty">More than one agent runs a session by this id, and tmag names none of them by it.</p>
        ) : gone ? (
          <p class="empty">This session is not running.</p>
        ) : mode === 'screen' ? (
          <Screen c={c} target={id} />
        ) : mode === 'changes' ? (
          <Changes c={c} id={id} s={s} put={() => setMode('turns')} />
        ) : !c.can('read') ? (
          <p class="empty">tmag on this computer does not read a session's turns.</p>
        ) : (
          <Turns c={c} id={id} s={s} say={say} />
        )}
      </Scroller>
      <Surface class="dock" layers="elevation acrylic">
        <div class="dock-in">
          {s && <Requests c={c} s={s} focus={request} say={say} folded={mode !== 'turns'} />}
          <p class={cls('toast', toast.error && 'error')} role="status">
            {toast.text}
          </p>
          {!gone &&
            !ambiguous &&
            (showScreen && c.can('keys') && c.screenOf(id).value.value?.screen ? (
              <KeyBar c={c} target={id} say={say} />
            ) : (
              <Composer c={c} id={id} s={s} screen={showScreen} say={say} />
            ))}
        </div>
      </Surface>
    </div>
  )
}

// Onward goes to the next session that waits on the person, in place of
// this one: back still leaves for where they came from.
function Onward({ c, id }: { c: Computer; id: string }) {
  const next = nextAfter(c, id)
  if (!next) return null
  const more = sections.value.waits.filter((e) => e.c !== c || e.s.id !== id).length
  return (
    <a
      class="onward"
      href={hash({ name: 'session', at: next.c.name, id: next.s.id })}
      aria-label={`Next that waits on you, of ${more}`}
      onClick={(e) => {
        e.preventDefault()
        go({ name: 'session', at: next.c.name, id: next.s.id }, { replace: true })
      }}
    >
      {more} waiting
      <Next />
    </a>
  )
}

// Ties are the sessions this one works for, and those it waits on, each a
// link to it.
function Ties({ c, s }: { c: Computer; s: S }) {
  const all = ties(c.view.value, s)
  if (!all.length) return null
  return (
    <div class="ties">
      {all.map((t) => (
        <a key={`${t.text}/${t.s.id}`} href={hash({ name: 'session', at: c.name, id: t.s.id })}>
          {t.text} {whereIs({ c, s: t.s })?.name ?? t.s.id.slice(0, 8)}
        </a>
      ))}
    </div>
  )
}
