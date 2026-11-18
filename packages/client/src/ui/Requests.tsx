import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Request, Session } from '../contract'
import { can } from '../view'
import { cls, explain, outcome } from './explain'
import { Markdown } from './Markdown'
import type { Say } from './Session'

type Decide = (decision: 'allow' | 'deny', extra?: { message?: string; answers?: Record<string, string> }) => void

// Requests shows a card for each request the session waits on that this page
// has not answered. A card keeps what is typed in it while the watch reports
// the session again. The request a link named comes into view, marked; one
// no longer open says so.
export function Requests({ c, s, focus, say }: { c: Computer; s: Session; focus?: string; say: Say }) {
  const done = c.answered.value
  const open = s.state === 'blocked' ? s.requests.filter((r) => !done.has(r.id)) : []
  const gone = !!focus && !open.some((r) => r.id === focus)
  // The request a link named stays in view as the cards before it read
  // what they ask and grow, until the person scrolls them.
  const cards = useRef<HTMLDivElement>(null)
  // biome-ignore lint/correctness/useExhaustiveDependencies: the cards it watches come and go with the open requests
  useLayoutEffect(() => {
    const el = cards.current
    if (!el || !focus) return
    let moved = false
    const keep = () => moved || el.querySelector('.card.focus')?.scrollIntoView({ block: 'nearest' })
    const grown = new ResizeObserver(keep)
    for (const card of el.children) grown.observe(card)
    const scrolled = () => {
      moved = true
    }
    el.addEventListener('touchmove', scrolled, { passive: true })
    el.addEventListener('wheel', scrolled, { passive: true })
    keep()
    return () => {
      grown.disconnect()
      el.removeEventListener('touchmove', scrolled)
      el.removeEventListener('wheel', scrolled)
    }
  }, [focus, open.length])
  if (!open.length && !gone) return null
  return (
    <div class="cards" ref={cards}>
      {gone && <p class="card muted">The request this link names is no longer open.</p>}
      {open.map((r) => (
        <Card key={r.id} c={c} s={s} r={r} focus={r.id === focus} say={say} />
      ))}
    </div>
  )
}

function Card({ c, s, r, focus, say }: { c: Computer; s: Session; r: Request; focus: boolean; say: Say }) {
  // A request is read again whenever the watch reports its session, where
  // its computer serves requests.
  useEffect(() => {
    if (c.can('request')) c.loadRequest(s.id, r.id)
  }, [c, s, r.id])
  const doc = c.requestOf(r.id).value
  const input = doc.value?.input
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const decide: Decide = async (decision, extra = {}) => {
    setBusy(true)
    setError('')
    try {
      await c.act({ verb: 'answer', target: s.id, request: r.id, decision, ...extra })
      say('Answered')
    } catch (e) {
      setError(outcome(e, 'answer'))
      setBusy(false)
    }
  }

  const heading =
    r.kind === 'question' ? 'Question' : r.kind === 'plan' ? 'Approve the plan?' : `Allow ${r.tool || 'this'}?`
  const answerable = can(s, 'answer') && c.can('answer')
  const questions = r.kind === 'question' ? asked(input) : undefined
  let body = null
  if (r.kind === 'question' && answerable) {
    body = questions ? (
      <Questions questions={questions} busy={busy} decide={decide} fail={setError} />
    ) : (
      <p class="muted">{doc.value || doc.error ? 'Answer it in its terminal.' : 'Reading the question…'}</p>
    )
  } else {
    body = (
      <>
        <What kind={r.kind} input={input} />
        {answerable ? (
          <Decision plan={r.kind === 'plan'} busy={busy} decide={decide} />
        ) : (
          <p class="muted">Answer it in its terminal.</p>
        )}
      </>
    )
  }

  return (
    <section class={cls('card', focus && 'focus')}>
      <h2>
        {heading}
        {r.agent && <small> · subagent</small>}
      </h2>
      {doc.error && <p class="muted">{explain(doc.error)}</p>}
      {body}
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
    </section>
  )
}

// What shows what a permission or a plan asks for: the plan, or the tool's
// input.
function What({ kind, input }: { kind: string; input: Record<string, unknown> | undefined }) {
  if (!input || kind === 'question') return null
  if (kind === 'plan' && typeof input.plan === 'string') return <Markdown class="plan" src={input.plan} />
  return <Asks input={input} />
}

function Decision({ plan, busy, decide }: { plan: boolean; busy: boolean; decide: Decide }) {
  const [reason, setReason] = useState('')
  return (
    <div class="actions">
      <input
        class="reason"
        aria-label="Reason"
        placeholder="Why not (optional, goes with Deny)"
        value={reason}
        disabled={busy}
        onInput={(e) => setReason(e.currentTarget.value)}
      />
      <button
        type="button"
        disabled={busy}
        onClick={() => decide('deny', reason.trim() ? { message: reason.trim() } : {})}
      >
        {plan ? 'Keep planning' : 'Deny'}
      </button>
      <button class="primary" type="button" disabled={busy} onClick={() => decide('allow')}>
        {plan ? 'Approve' : 'Allow'}
      </button>
    </div>
  )
}

// Asks shows what a permission request would do: a command, a file or an
// address first, then all of its input.
function Asks({ input }: { input: Record<string, unknown> }) {
  const command = Array.isArray(input.command) ? input.command.join(' ') : input.command
  const target = input.file_path ?? input.path ?? input.url ?? input.pattern
  const shown = typeof command === 'string' ? command : typeof target === 'string' ? target : undefined
  return (
    <div class="asks">
      {typeof input.description === 'string' && <p>{input.description}</p>}
      {shown && <pre>{shown}</pre>}
      <details>
        <summary>Input</summary>
        <pre>{JSON.stringify(input, null, 2)}</pre>
      </details>
    </div>
  )
}

interface Question {
  question: string
  header?: string
  multiSelect?: boolean
  options?: { label: string; description?: string }[]
}

// asked is the questions an AskUserQuestion input holds, if it holds any.
function asked(input: Record<string, unknown> | undefined): Question[] | undefined {
  const qs = input?.questions
  if (!Array.isArray(qs) || !qs.length) return undefined
  return qs.every((q) => typeof q?.question === 'string') ? (qs as Question[]) : undefined
}

function Questions(props: { questions: Question[]; busy: boolean; decide: Decide; fail: (why: string) => void }) {
  const { questions, busy, decide, fail } = props
  const [chosen, setChosen] = useState<string[][]>(() => questions.map(() => []))
  const [other, setOther] = useState<string[]>(() => questions.map(() => ''))

  const pick = (at: number, label: string, many: boolean | undefined) =>
    setChosen((all) =>
      all.map((c, i) => {
        if (i !== at) return c
        if (!many) return [label]
        return c.includes(label) ? c.filter((l) => l !== label) : [...c, label]
      }),
    )

  // Every question needs an answer: the one written, or the options picked.
  const submit = () => {
    const answers: Record<string, string> = {}
    for (const [i, q] of questions.entries()) {
      const answer = other[i]?.trim() || chosen[i]?.join(', ')
      if (!answer) return fail('Answer each question.')
      answers[q.question] = answer
    }
    decide('allow', { answers })
  }

  return (
    <>
      {questions.map((q, i) => (
        <fieldset key={q.question} disabled={busy}>
          <legend>
            {q.header && <span class="tag">{q.header}</span>}
            {q.question}
          </legend>
          {q.options?.map((o) => {
            const on = chosen[i]?.includes(o.label) ?? false
            return (
              <button
                key={o.label}
                type="button"
                class={cls('option', on && 'on')}
                aria-pressed={on}
                onClick={() => pick(i, o.label, q.multiSelect)}
              >
                <strong>{o.label}</strong>
                {o.description && <small>{o.description}</small>}
              </button>
            )
          })}
          <input
            class="other"
            aria-label="Other answer"
            placeholder="Other answer"
            value={other[i]}
            onInput={(e) => {
              const value = e.currentTarget.value
              setOther((all) => all.map((o, j) => (j === i ? value : o)))
            }}
          />
        </fieldset>
      ))}
      <div class="actions">
        <button type="button" disabled={busy} onClick={() => decide('deny')}>
          Dismiss
        </button>
        <button class="primary" type="button" disabled={busy} onClick={submit}>
          Answer
        </button>
      </div>
    </>
  )
}
