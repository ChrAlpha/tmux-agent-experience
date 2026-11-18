import { useEffect, useState } from 'preact/hooks'
import type { Request, Session } from '../contract'
import { act, answered, loadRequest, requestOf } from '../store'
import { can } from '../view'
import { cls, explain, outcome } from './explain'
import { Markdown } from './Markdown'
import type { Say } from './Session'

type Decide = (decision: 'allow' | 'deny', extra?: { message?: string; answers?: Record<string, string> }) => void

// Requests shows a card for each request the session waits on that this page
// has not answered. A card keeps what is typed in it while the watch reports
// the session again.
export function Requests({ s, say }: { s: Session; say: Say }) {
  const done = answered.value
  const open = s.state === 'blocked' ? s.requests.filter((r) => !done.has(r.id)) : []
  if (!open.length) return null
  return (
    <div class="cards">
      {open.map((r) => (
        <Card key={r.id} s={s} r={r} say={say} />
      ))}
    </div>
  )
}

function Card({ s, r, say }: { s: Session; r: Request; say: Say }) {
  useEffect(() => loadRequest(s.id, r.id), [s.id, r.id])
  const doc = requestOf(r.id).value
  const input = doc.value?.input
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const decide: Decide = async (decision, extra = {}) => {
    setBusy(true)
    setError('')
    try {
      await act({ verb: 'answer', target: s.id, request: r.id, decision, ...extra })
      say('Answered')
    } catch (e) {
      setError(outcome(e, 'answer'))
      setBusy(false)
    }
  }

  const heading =
    r.kind === 'question' ? 'Question' : r.kind === 'plan' ? 'Approve the plan?' : `Allow ${r.tool || 'this'}?`
  const answerable = can(s, 'answer')
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
    <section class="card">
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
