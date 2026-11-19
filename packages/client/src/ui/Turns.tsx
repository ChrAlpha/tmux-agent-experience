import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Prompt, Session as S } from '../contract'
import { status, unwritten } from '../view'
import { Surface } from './axiom'
import { cls, explain } from './explain'
import { Copy } from './icons'
import { Markdown } from './Markdown'
import { useFollow } from './Scroller'
import type { Say } from './toast'

// Turns are a session's last turns: what it was given and what it
// answered, then what the page sent it that they do not show yet.
export function Turns({ c, id, s, say }: { c: Computer; id: string; s: S | undefined; say: Say }) {
  const read = c.turnsOf(id).value
  const sent = c.sentTo(id).value
  const follow = useFollow()
  const before = useRef(sent.length)
  // biome-ignore lint/correctness/useExhaustiveDependencies: what is read, the state, or what was sent changes what shows; a prompt the person unfolds does not move the scroller
  useLayoutEffect(() => {
    // What the person just sent brings them to the end.
    follow(sent.length > before.current)
    before.current = sent.length
  }, [follow, read, s?.state, sent])
  const turns = read.value
  // What the transcript holds may lag what tmag reported, and no event
  // follows: the reply of a turn an idle session ended, which Claude Code
  // may write after its Stop hook, or a prompt sent that no turn shows yet.
  // The turns are read again every second, three times for each.
  const lag = useRef({ key: '', left: 0 })
  useEffect(() => {
    if (!turns || !(sent.length || unwritten(s, turns))) return
    const key = `${s?.turn}/${sent[sent.length - 1]?.id}`
    if (lag.current.key !== key) lag.current = { key, left: 3 }
    if (!lag.current.left) return
    const again = setTimeout(() => {
      lag.current.left--
      c.loadTurns(id)
    }, 1000)
    return () => clearTimeout(again)
  }, [c, id, s, turns, sent])
  const shown = sent.map((p) => <PromptText key={`sent-${p.id}`} p={{ text: p.text }} sent />)
  if (!turns) {
    return (
      <div class="timeline">
        <p class={cls('empty', read.error && 'error')}>{read.error ? explain(read.error) : 'Reading its turns…'}</p>
        {shown}
      </div>
    )
  }
  // The turn that runs is the last one read, unless tmag names another,
  // which the transcript does not show yet.
  const working = s && s.state !== 'idle' ? s : undefined
  const last = turns[turns.length - 1]
  const runsLast = working && last && (!working.turn || working.turn === last.id)
  return (
    <div class="timeline">
      {turns.map((turn, i) => {
        const running = runsLast && turn === last
        return (
          <article class="turn" key={turn.id || i}>
            {turn.prompts.map((p, j) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: a turn's prompts only grow at its end
              <PromptText key={j} p={p} />
            ))}
            {turn.reply ? <Reply text={turn.reply} say={say} /> : !running && <p class="muted no-reply">No reply</p>}
            {running && <Running s={working} />}
          </article>
        )
      })}
      {!turns.length && !working && !sent.length && <p class="empty">No turns yet.</p>}
      {working && !runsLast && <Running s={working} />}
      {shown}
      {read.error && <p class="muted error">{explain(read.error)}</p>}
    </div>
  )
}

// Running is what a session does in the turn it runs: its progress, as
// Windows Phone's dots, while it works.
const Running = ({ s }: { s: S }) => (
  <p class={`running ${s.state}`}>
    {s.state === 'working' && (
      <span class="dots" aria-hidden="true">
        <i />
        <i />
        <i />
        <i />
        <i />
      </span>
    )}
    {status(s)}…
  </p>
)

// Reply is what an agent answered, as Markdown, with a way to copy it as it
// wrote it, where the browser lets the page write to the clipboard.
function Reply({ text, say }: { text: string; say: Say }) {
  const copies = typeof navigator.clipboard?.writeText === 'function'
  return (
    <div class="reply">
      <Markdown src={text} />
      {copies && (
        <button
          class="icon-button copy"
          type="button"
          aria-label="Copy the reply"
          onClick={() =>
            navigator.clipboard.writeText(text).then(
              () => say('Copied'),
              () => say('The browser did not let the page copy it.', true),
            )
          }
        >
          <Copy />
        </button>
      )}
    </div>
  )
}

// PromptText is a prompt of a turn, or one the page sent that no turn shows
// yet, as a message with a tail: the person's from the right, another's,
// an agent's or a task's, from the left.
function PromptText({ p, sent = false }: { p: Prompt; sent?: boolean }) {
  const [open, setOpen] = useState(false)
  const long = p.text.length > 600 || p.text.split('\n').length > 12
  const who = sent ? 'sent' : p.from === 'peer' ? 'from an agent' : p.from === 'task' ? 'background task' : p.from
  return (
    <Surface class={cls('prompt', p.from && 'from', sent && 'sent', long && !open && 'folded')} layers="fill">
      <div class="text">
        {who && <span class="who">{who}</span>}
        {p.text || '(no text)'}
      </div>
      {long && (
        <button class="more" type="button" onClick={() => setOpen(!open)}>
          {open ? 'Show less' : 'Show all'}
        </button>
      )}
    </Surface>
  )
}
