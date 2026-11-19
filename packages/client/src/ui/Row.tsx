import { useEffect, useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Request, Session, Turn } from '../contract'
import { snippet } from '../places'
import { hash } from '../route'
import { computers, type Entry, needs, now, title, whereIs } from '../store'
import { agentName, ago, can, helpers, status, ties, unwritten } from '../view'
import { Lamp, native } from './axiom'
import { cls, outcome } from './explain'
import { Branch } from './icons'
import { ask, asked, shown, whole } from './Requests'

// Row is a session in a list: where it runs (its project in the inbox, the
// agent and where in the project among a project's sessions), what it does,
// and what it last said or was asked. A permission it waits on shows what
// it would do, and takes Allow or Deny there; anything else it asks opens
// the session.
export function Row({ e, in: inProject = false }: { e: Entry; in?: boolean }) {
  const { c, s } = e
  const w = whereIs(e)
  const waits = needs(e)
  const several = computers.value.length > 1
  // A session's last turn is read for the list as its state or turn changes:
  // what it was asked as it begins, and what it answered as it ends. A
  // transcript that does not show yet the turn tmag reports, or the reply to
  // it once it ended, as Claude Code may write after its Stop hook, is read
  // again every second, three times.
  const at = `${s.state}/${s.kind}/${s.turn}`
  const last = c.latest(s.id)
  const late = !!s.turn && last !== undefined && (!current(s, last) || (!!last && unwritten(s, [last])))
  useEffect(() => {
    if (c.can('read')) c.loadLast(s.id, at)
  }, [c, s.id, at])
  useEffect(() => {
    if (!late || !c.can('read')) return
    let n = 0
    const again = setInterval(() => {
      if (++n > 3) clearInterval(again)
      else c.loadLast(s.id, `${at}/${n}`)
    }, 1000)
    return () => clearInterval(again)
  }, [c, s.id, at, late])
  const name = inProject ? agentName(s.agent) : (w?.name ?? s.id.slice(0, 8))
  const place = w && [w.worktree || inProject ? w.branch : '', w.sub].filter(Boolean).join(' · ')
  const tie = ties(c.view.value, s)[0]
  const meta = [
    !inProject && agentName(s.agent),
    s.state === 'idle' && waits ? 'done' : status(s),
    tie && `${tie.text} ${whereIs({ c, s: tie.s })?.name ?? tie.s.id.slice(0, 8)}`,
    helpers(s),
    several && title(c),
  ]
  const request = s.state === 'blocked' ? s.requests.find((r) => !c.answered.value.has(r.id)) : undefined
  return (
    <li class={cls('row', s.state, waits && 'waits', waits && s.state === 'idle' && 'done')}>
      <ax-pressure class="row-press" onDragStartCapture={native}>
        <Lamp />
        <a class="row-link" href={hash({ name: 'session', at: c.name, id: s.id })}>
          <span class="mark" aria-hidden="true">
            {mark(s.agent)}
          </span>
          <span class="body">
            <span class="line1">
              <span class="name">{name}</span>
              {place && (
                <span class={cls('chip', w?.worktree && 'tree')}>
                  {w?.branch && (w.worktree || inProject) && <Branch />}
                  {place}
                </span>
              )}
              <span class="when">{ago(s.since, now.value)}</span>
            </span>
            <span class="meta">{meta.filter(Boolean).join(' · ')}</span>
            {request ? <Asking c={c} s={s} r={request} /> : <Said c={c} s={s} full={waits} />}
          </span>
        </a>
      </ax-pressure>
      {request && <Quick key={request.id} c={c} s={s} r={request} />}
    </li>
  )
}

// The letters of an agent on the square of its state, as a tile names an app.
const marks: Record<string, string> = { claude: 'CC', codex: 'Cx', pi: 'Pi', omp: 'Op' }

// mark is the letters of an agent: its own, or the first two of its name.
const mark = (agent: string) => marks[agent] ?? agentName(agent).slice(0, 2)

// current is the last turn read when it is the turn tmag reports the session
// in, or tmag names none: a list never shows an older turn's answer as the
// latest.
const current = (s: Session, last: Turn | null): Turn | null => (last && (!s.turn || last.id === s.turn) ? last : null)

// Said is what a session was last asked while it works, and what it last
// answered once it is idle.
function Said({ c, s, full }: { c: Computer; s: Session; full: boolean }) {
  const last = current(s, c.latest(s.id) ?? null)
  if (!last) return null
  const prompt = last.prompts[last.prompts.length - 1]?.text ?? ''
  const text = s.state === 'idle' && last.reply ? last.reply : prompt
  if (!text) return null
  return <span class={cls('said', s.state !== 'idle' && 'asked', full && 'full')}>{snippet(text)}</span>
}

// Asking is what a session asks the person, read as its request is.
function Asking({ c, s, r }: { c: Computer; s: Session; r: Request }) {
  useEffect(() => {
    if (c.can('request')) c.loadRequest(s.id, r.id)
  }, [c, s, r.id])
  const read = c.requestOf(r.id).value
  const input = read.value?.input
  const question = r.kind === 'question' ? asked(input)?.[0]?.question : undefined
  const all = !read.error && (r.kind === 'permission' || !r.kind) ? whole(input) : undefined
  const what = all ?? (r.kind === 'permission' || !r.kind ? shown(input) : undefined)
  return (
    <span class="asks">
      <span class="ask">{question ?? ask(r)}</span>
      {what && <code class={cls(all && 'whole')}>{what}</code>}
    </span>
  )
}

// Quick answers in the list a permission to run a command the row shows in
// full, where tmag can answer it. Anything else it asks, or a permission
// the row cannot show all of, the person answers on the request itself.
function Quick({ c, s, r }: { c: Computer; s: Session; r: Request }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // What could not be read again may be out of date: it is answered on the request.
  const read = c.requestOf(r.id).value
  const command = read.error ? undefined : whole(read.value?.input)
  const permission = r.kind === 'permission' || !r.kind
  if (!permission || !command || !can(s, 'answer') || !c.can('answer')) {
    return (
      <div class="quick">
        <a class="button" href={hash({ name: 'session', at: c.name, id: s.id, request: r.id })}>
          {r.kind === 'question' ? 'Answer' : 'Review'}
        </a>
      </div>
    )
  }
  const decide = async (decision: 'allow' | 'deny') => {
    setBusy(true)
    setError('')
    try {
      await c.act({ verb: 'answer', target: s.id, request: r.id, decision })
    } catch (e) {
      setError(outcome(e, 'answer'))
      setBusy(false)
    }
  }
  return (
    <div class="quick">
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      <button type="button" disabled={busy} onClick={() => decide('deny')}>
        Deny
      </button>
      <button class="primary" type="button" disabled={busy} onClick={() => decide('allow')}>
        Allow
      </button>
    </div>
  )
}
