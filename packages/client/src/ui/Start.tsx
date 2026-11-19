import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { Failure } from '../api'
import type { Computer } from '../computer'
import { type Action, type StartDoc, startable } from '../contract'
import { tilde, where } from '../places'
import { hash, type Route } from '../route'
import {
  computers,
  go,
  holding,
  instanceOf,
  kept as keptAt,
  later,
  named,
  startEdits,
  startText,
  title,
} from '../store'
import { agentName, sessions } from '../view'
import { Surface } from './axiom'
import { chosen, DirPicker, missing } from './DirPicker'
import { cls, explain, outcome, refusal } from './explain'
import { useFrame } from './frame'
import { Header } from './Header'
import { useVisit } from './visit'

type Who = { agent: (typeof startable)[number] } | { template: string }

/** A computer the sheet works on, as the instance it was when the sheet showed it. */
interface Pin {
  c: Computer
  instance?: string
}

const pin = (c: Computer): Pin => {
  const instance = instanceOf(c)
  return { c, ...(instance ? { instance } : {}) }
}

/** What the sheet shows and keeps, for one start link. */
interface Sheet {
  /** The link it was opened at: a computer's address and a directory. */
  at: string
  linked?: string
  /** Whether a computer it shows changed what it is while it was open. */
  changed: boolean
  /** The computer it works on, once it showed one. */
  pinned: Pin | null
  /** The directory, once it showed one or the person typed one. */
  typed: string | null
  /** Whether the picker is open, once the person said. */
  opened: boolean | null
  /** The agent, once it showed one or the person picked one. */
  who: Who | null
  /** Whether the person picked the agent. */
  chose: boolean
  template: string
  branch: string | null
}

const opening = (at: string, linked: string | undefined): Sheet => {
  return {
    at,
    ...(linked ? { linked } : {}),
    changed: false,
    pinned: null,
    typed: linked ?? null,
    opened: linked ? false : null,
    who: null,
    chose: false,
    template: '',
    branch: null,
  }
}

const templateName = /^[a-z0-9][a-z0-9_-]{0,31}$/

// Start starts an agent on a computer, in a window of its own beside tmag
// serve's, in a directory the person chooses, with an agent by its name or
// a template the person set there, and gives it a first prompt as send
// does. It works on the computer its link names, once that one said it
// starts agents, and only there; for a link that names neither a computer
// nor a directory, on the one that serves the page where it starts agents,
// else the first that does. What it first shows (the computer, then the
// directory once the computer said where its agents ran, and the agent) it
// keeps until the person changes it, and from the person's first act on it.
// If the computer it shows changes what it is while it is open, or the page
// moves its link to another address, it starts nothing until the person
// starts over: it never turns to another computer by itself.
export function Start({ at, dir: linked }: { at: string; dir?: string }) {
  const all = computers.value
  const able = all.filter((x) => x.can('start'))
  const [s, setSheet] = useState<Sheet>(() => opening(at, linked))
  const set = (part: Partial<Sheet>) => setSheet((now) => ({ ...now, ...part }))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // How many times the person was told what is wrong, each one brought into view.
  const [told, setTold] = useState(0)
  // The session a start that ended in part made, on the instance it started on.
  const [started, setStarted] = useState<{ go: Route; of: Computer; instance?: string } | null>(null)
  // How many times the person turned to another computer: a start begun
  // before the last turn ends in the bar, not on the sheet of another.
  const turned = useRef(0)
  // A start can take a while: what it ends in goes to the sheet while the
  // person stays on it, and to a bar once they left it.
  const visit = useVisit()
  const frame = useFrame()
  // What went wrong comes into view, wherever the sheet was scrolled.
  const said = useRef<HTMLParagraphElement>(null)
  // biome-ignore lint/correctness/useExhaustiveDependencies: each time the person is told, even the same again
  useEffect(() => {
    if (error) said.current?.scrollIntoView({ block: 'nearest' })
  }, [error, told])

  // Another start link opens another sheet. One the page moved, as it forgot
  // the address it named as another of a computer it works with, is a sheet
  // whose computers changed.
  const moved = at !== s.at || linked !== s.linked
  const rewritten = moved && linked === s.linked && !named(s.at) && keptAt(s.at) === at
  const over = () => {
    turned.current++
    setSheet(opening(at, linked))
    setError('')
    setStarted(null)
  }
  useLayoutEffect(() => {
    if (!moved) return
    if (rewritten) {
      turned.current++
      set({ at, changed: true })
    } else over()
  })

  const routed = named(at)
  const strict = !!at || !!linked
  const p = s.pinned
  // The computer it pinned, while the page works with it and it is the same instance.
  const kept = p && all.includes(p.c) && instanceOf(p.c) === p.instance ? p.c : undefined
  const name = p
    ? kept?.name
    : routed?.can('start')
      ? routed.name
      : strict
        ? routed && !routed.info.value
          ? null
          : undefined
        : able[0]?.name
  const c = able.find((x) => x.name === name)
  // The sheet keeps the computer it shows, and the directory and agent once
  // the computer said where its agents ran; never a directory it does not
  // show yet.
  useEffect(() => {
    // A sheet whose computer changed stays so, whatever comes at its address.
    if (p && !kept && !s.changed) return set({ changed: true })
    if (!c || moved || s.changed) return
    if (!s.pinned) return set({ pinned: pin(c) })
    const d = derive(c, s)
    const part = d.pending ? {} : settle(s, d)
    if (Object.keys(part).length) set(part)
  })

  if (s.changed || (p && !kept)) {
    return (
      <div class="screen sheet" ref={frame}>
        <Header title="New agent" />
        <main class="scroll">
          <div class="empty">
            <p>The computers changed while this sheet was open.</p>
            <p class="muted">
              {p && !kept
                ? `${p.c.name ? `/${p.c.name}/` : location.host} is not the computer it showed any more. `
                : 'The page moved its link to another address of the same computer. '}
              It starts nothing there until you look again. Your first prompt stays.
            </p>
            <button class="primary" type="button" onClick={over}>
              Start over
            </button>
          </div>
        </main>
      </div>
    )
  }

  if (!c) {
    const waits = name === null ? routed : strict || p ? undefined : all.find((x) => !x.info.value)
    return (
      <div class="screen sheet" ref={frame}>
        <Header title="New agent" />
        <main class="scroll">
          <p class="empty">
            {waits
              ? `Asking ${title(waits)} what it serves…${waits.problem.value ? ` ${explain(waits.problem.value)}` : ''}`
              : routed?.info.value && !routed.info.value.instance
                ? `${title(routed)} does not say which computer it is, so the page starts no agent there.`
                : routed
                  ? `tmag on ${title(routed)} is older than tmag start, so the page starts no agent there${linked ? ` in ${linked}` : ''}.`
                  : 'No computer the page works with starts agents: its tmag is older than tmag start.'}
          </p>
        </main>
      </div>
    )
  }

  const several = all.length > 1
  const d = derive(c, s)
  const { home, value, dir, who, pending } = d
  const open = s.opened ?? !value
  const repo = dir.startsWith('/') ? c.repoAt(dir) : undefined
  const lists = c.can('dirs')
  // A worktree is offered where the directory is in a repository, or where
  // the computer does not say whether it is; one the person asked for stays,
  // for them to take back.
  const offered = !!repo || !lists || s.branch !== null
  const templates = c.places.value.value?.templates ?? []
  const text = startText.value
  // Until it said whether a directory is one, the sheet does not start an
  // agent there.
  const listed = lists && dir.startsWith('/') ? c.dirOf(dir).value : undefined
  const checking = !!listed && !listed.value && !listed.error
  // What the person does on the sheet keeps what it shows.
  const act = (part: Partial<Sheet> = {}) => set({ ...settle(s, d), ...part })
  // The session a start made in part opens at the address the page keeps of
  // the instance it started on.
  const madeOn = started ? holding(started.instance, started.of) : undefined
  const madeAt = started && 'at' in started.go && madeOn ? { ...started.go, at: madeOn.name } : undefined
  const tell = (problem: string) => {
    setError(problem)
    setTold((n) => n + 1)
  }

  async function submit(ev: Event) {
    ev.preventDefault()
    if (!c) return
    const tpl = 'template' in who ? who.template || s.template.trim() : ''
    const worktree = s.branch?.trim() ?? ''
    const problem = !dir.startsWith('/')
      ? 'Choose a directory, or give it as an absolute path.'
      : lists && missing(c, dir)
        ? `${tilde(dir, home)} is no directory on ${title(c)}.`
        : 'template' in who && !templateName.test(tpl)
          ? 'A template is named by lowercase letters, digits, - and _, as @tmag-start-NAME names it.'
          : s.branch !== null && !worktree
            ? 'Name the branch its worktree is for.'
            : ''
    if (problem) return tell(problem)
    act()
    const prompt = text.trim() ? text : ''
    const action: Action = {
      verb: 'start',
      ...('template' in who ? { template: tpl } : { agent: who.agent }),
      dir,
      ...(worktree ? { worktree } : {}),
      ...(prompt ? { text: prompt } : {}),
    }
    const what = `${'template' in who ? `@tmag-start-${tpl}` : agentName(who.agent)} in ${tilde(dir, home)}${several ? ` on ${title(c)}` : ''}`
    // What it ends in belongs to the instance it starts on, at whichever
    // address the page keeps of it then, and shows on the sheet only while
    // the person is still there on that computer.
    const on = { instance: instanceOf(c), c }
    const turn = turned.current
    setBusy(true)
    setError('')
    setStarted(null)
    const visited = visit()
    const v = {
      shown: visited.shown,
      here: () => visited.here() && turn === turned.current && holding(on.instance, on.c) === on.c,
    }
    // left says what became of the start in a bar, once the person left
    // where they began it; a sheet still showing is ready for another.
    const left = (said: string, to?: Route, label?: string) => {
      later.value = {
        text: `${what}: ${said}`,
        ...(to && { go: to, label, of: on.c, ...(on.instance ? { instance: on.instance } : {}) }),
      }
      setBusy(false)
    }
    // The prompt leaves the sheet once the agent has it, unless the person
    // wrote in it since.
    const edits = startEdits.value
    const gave = () => {
      if (prompt && startEdits.value === edits) startText.value = ''
    }
    const ended = (e: unknown) => {
      const r = after(holding(on.instance, on.c), e, prompt)
      if (!v.here()) left(r.text, r.go, r.label)
      else if (r.go?.name === 'pane') go(r.go, { replace: true })
      else {
        tell(r.text)
        setStarted(
          r.go?.name === 'session' ? { go: r.go, of: on.c, ...(on.instance ? { instance: on.instance } : {}) } : null,
        )
        setBusy(false)
      }
    }
    try {
      const doc = await c.act<StartDoc>(action)
      if (!doc?.session?.id) return ended(new Failure(200, 'network', 'its answer was cut short'))
      gave()
      const now = holding(on.instance, on.c)
      const to: Route | undefined = now && { name: 'session', at: now.name, id: doc.session.id }
      if (v.here() && to) go(to, { replace: true })
      else left('it started.', to, 'Open it')
    } catch (e) {
      ended(e)
    }
  }

  const agents = startable.map((a) => ({ key: a, label: agentName(a), who: { agent: a } as Who }))
  const presets = lists
    ? templates.map((t) => ({
        key: `@${t.name}`,
        label: `${t.name} · ${agentName(t.agent)}`,
        who: { template: t.name },
      }))
    : [{ key: '@', label: 'A template', who: { template: '' } as Who }]
  const on = (w: Who) =>
    'agent' in w ? 'agent' in who && who.agent === w.agent : 'template' in who && who.template === w.template

  return (
    <form class="screen sheet" ref={frame} onSubmit={submit}>
      <Header title="New agent" meta={`on ${title(c)}`} />
      <main class="scroll">
        {several && (
          <fieldset class="choices">
            <legend>Computer</legend>
            <div class="chips">
              {able.map((x) => (
                <label key={x.name} class={cls('choice', x === c && 'on')}>
                  <input
                    type="radio"
                    name="computer"
                    checked={x === c}
                    onClick={() => {
                      // Another computer starts from its own defaults, or
                      // from the directory the link named on it.
                      if (x === c) return set({ pinned: pin(x) })
                      turned.current++
                      const back = x === routed && linked
                      set({
                        pinned: pin(x),
                        typed: back ? linked : null,
                        opened: back ? false : null,
                        who: null,
                        chose: false,
                        template: '',
                        branch: null,
                      })
                      setError('')
                      setStarted(null)
                    }}
                  />
                  {title(x)}
                </label>
              ))}
            </div>
          </fieldset>
        )}
        <fieldset class="choices">
          <legend>Where</legend>
          <DirPicker
            key={instanceOf(c) ?? c.name}
            c={c}
            value={value}
            onChange={(t) => act({ typed: t, opened: true })}
            onChoose={(d) => {
              if (!s.chose) set({ who: { agent: usual(c, d) } })
            }}
            open={open}
            setOpen={(o) => act({ opened: o })}
          />
          {offered && (
            <label class="check">
              <input
                type="checkbox"
                checked={s.branch !== null}
                onChange={(e) => act({ branch: e.currentTarget.checked ? '' : null })}
              />
              In a worktree of its own
            </label>
          )}
          {s.branch !== null && (
            <input
              class="field mono"
              aria-label="Branch"
              placeholder={`A branch${repo ? ` of ${where(dir, repo).name}` : ''}, new or not`}
              autocapitalize="none"
              autocomplete="off"
              spellcheck={false}
              value={s.branch}
              onInput={(e) => act({ branch: e.currentTarget.value })}
            />
          )}
        </fieldset>
        <fieldset class="choices">
          <legend>Agent</legend>
          <div class="chips">
            {[...agents, ...presets].map((a) => (
              <label key={a.key} class={cls('choice', on(a.who) && 'on')}>
                <input type="radio" name="who" checked={on(a.who)} onClick={() => act({ who: a.who, chose: true })} />
                {a.label}
              </label>
            ))}
          </div>
          {'template' in who && !lists && (
            <input
              class="field mono"
              aria-label="Template"
              placeholder="Its name, as @tmag-start-NAME names it"
              autocapitalize="none"
              spellcheck={false}
              value={s.template}
              onInput={(e) => act({ template: e.currentTarget.value })}
            />
          )}
        </fieldset>
        <label class="choices">
          <span class="legend">Prompt</span>
          <textarea
            class="field"
            aria-label="Prompt"
            rows={5}
            placeholder="What it should work on. Optional: it waits for one otherwise."
            value={text}
            onInput={(e) => {
              act()
              startText.value = e.currentTarget.value
              startEdits.value++
            }}
          />
        </label>
        {error && (
          <p class="error" role="alert" ref={said}>
            {error}
            {madeAt && (
              <>
                {' '}
                <a href={hash(madeAt)}>Open it</a>
              </>
            )}
          </p>
        )}
      </main>
      <Surface class="dock" layers="elevation acrylic">
        <div class="foot">
          <button class="primary wide" type="submit" disabled={busy || pending || checking}>
            {busy ? 'Starting…' : 'Start'}
          </button>
        </div>
      </Surface>
    </form>
  )
}

// derive is what a sheet shows on a computer: the directory it keeps or
// would start in, the agent, and whether the directory is not known yet,
// until the computer said where its agents ran.
function derive(c: Computer, s: Sheet) {
  const home = c.places.value.value?.home
  const value = s.typed ?? tilde(lastPlace(c) ?? '', home)
  const dir = chosen(value, home)
  const who = s.who ?? { agent: usual(c, dir) }
  const pending = s.typed === null && (c.can('dirs') ? !c.places.value.value && !c.places.value.error : !c.loaded.value)
  return { home, value, dir, who, pending }
}

// settle is what of a sheet's derived directory and agent it keeps: those
// it shows, never a directory it does not show yet.
function settle(s: Sheet, d: ReturnType<typeof derive>): Partial<Sheet> {
  if (!d.value) return {}
  return { ...(s.typed === null ? { typed: d.value } : {}), ...(s.who === null ? { who: d.who } : {}) }
}

// lastPlace is where an agent of the computer ran last: the place it names
// first, where it names places, else where its latest session runs.
function lastPlace(c: Computer): string | undefined {
  const first = c.places.value.value?.recent[0]
  if (first) return first.dir
  return sessions(c.view.value).find((s) => s.cwd)?.cwd
}

// usual is the agent that runs most of the computer's sessions in a
// directory, else most of its sessions, else Claude Code.
function usual(c: Computer, dir: string): (typeof startable)[number] {
  const all = [...c.view.value.sessions.values()]
  const count = (list: typeof all) => {
    const n = new Map<string, number>()
    for (const s of list) n.set(s.agent, (n.get(s.agent) ?? 0) + 1)
    return [...n].sort((a, b) => b[1] - a[1]).find(([a]) => (startable as readonly string[]).includes(a))?.[0]
  }
  const here = all.filter((s) => s.cwd && dir && (s.cwd === dir || s.cwd.startsWith(`${dir}/`)))
  return (count(here) ?? count(all) ?? 'claude') as (typeof startable)[number]
}

// after says what a start that failed ended in, and where to see it, on the
// computer it started on, where the page still works with it: the pane of
// an agent that shows no session yet, which holds its prompt; the session
// of one that started and whose prompt then failed, whose message holds the
// prompt when tmag refused to give it, and only then, since any other
// failure may follow its delivery.
function after(c: Computer | undefined, e: unknown, prompt: string): { text: string; go?: Route; label?: string } {
  const doc = e instanceof Failure ? e.doc : undefined
  if (doc?.pane) {
    const pane = `${doc.pane.server}:${doc.pane.id}`
    if (!c) return { text: 'it shows no session yet, on a computer the page no longer works with.' }
    if (prompt) c.draftOf(pane).value = prompt
    return {
      text: 'it shows no session yet, and may ask something first.',
      go: { name: 'pane', at: c.name, pane },
      label: 'Open its pane',
    }
  }
  if (doc?.started) {
    if (!c) return { text: `It started, on a computer the page no longer works with. tmag says: ${explain(e)}` }
    const go: Route = { name: 'session', at: c.name, id: doc.started.id }
    const refused = refusal(e, 'send')
    if (!prompt) return { text: `It started. tmag says: ${explain(e)}`, go, label: 'Open it' }
    if (refused === undefined) {
      return {
        text: `It started, and whether it got the prompt is unknown: look at it before sending the prompt again. tmag says: ${explain(e)}`,
        go,
        label: 'Open it',
      }
    }
    const into = c.draftOf(doc.started.id)
    into.value = into.value ? `${into.value}\n\n${prompt}` : prompt
    return {
      text: `It started, but tmag did not give it the prompt, which waits in its message: ${refused}`,
      go,
      label: 'Open it',
    }
  }
  return { text: outcome(e, 'start') }
}
