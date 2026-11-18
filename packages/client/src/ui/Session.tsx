import type { ComponentChildren } from 'preact'
import { createContext } from 'preact'
import { useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import type { Prompt, Session as S } from '../contract'
import { act, back, loaded, loadScreen, loadTurns, now, screenOf, sentTo, turnsOf, view } from '../store'
import { ago, can, find, follows, helpers, paneOf, place, status, stoppable, unwritten } from '../view'
import { cls, explain, outcome } from './explain'
import { Markdown } from './Markdown'
import { Requests } from './Requests'

export type Say = (text: string, error?: boolean) => void

export function Session({ id }: { id: string }) {
  const v = view.value
  const s = find(v, id)
  const pane = s && paneOf(v, s)
  const [screen, setScreen] = useState(false)
  const showScreen = screen && !!pane
  const gone = loaded.value && !s
  const [toast, say] = useToast()

  // What the session shows is read again whenever the watch reports it: the
  // view makes a new object of each session an event or a snapshot reports.
  useEffect(() => {
    if (s) (showScreen ? loadScreen : loadTurns)(id)
  }, [id, s, showScreen])
  // A screen also changes with nothing to report, as an agent writes: it is
  // read again every two seconds while it shows.
  useEffect(() => {
    if (!showScreen) return
    const again = setInterval(() => document.hidden || loadScreen(id), 2000)
    return () => clearInterval(again)
  }, [id, showScreen])

  return (
    <div class="screen session">
      <header>
        <button class="back" type="button" aria-label="Agents" onClick={back}>
          ‹
        </button>
        <div class="what">
          <h1 class="name">{s ? place(s) : 'Session'}</h1>
          <div class="meta">
            {s
              ? [s.agent, status(s), helpers(s), ago(s.since, now.value)].filter(Boolean).join(' · ')
              : gone
                ? 'not running'
                : ''}
          </div>
        </div>
        {pane && (
          <button class="mode" type="button" onClick={() => setScreen(!showScreen)}>
            {showScreen ? 'Turns' : 'Screen'}
          </button>
        )}
      </header>
      <Scroller key={showScreen ? 'screen' : 'turns'}>
        {gone ? (
          <p class="empty">This session is not running.</p>
        ) : showScreen ? (
          <Screen id={id} />
        ) : (
          <Turns id={id} s={s} />
        )}
      </Scroller>
      {s && <Requests s={s} say={say} />}
      <p class={cls('toast', toast.error && 'error')} role="status">
        {toast.text}
      </p>
      {!gone && <Composer id={id} s={s} screen={showScreen} say={say} />}
    </div>
  )
}

// Follow keeps a scroller at its end when what it shows changes, as long as
// the person has not scrolled away from it, or brings it there.
const Follow = createContext((_pin?: boolean) => {})

function Scroller({ children }: { children: ComponentChildren }) {
  const ref = useRef<HTMLElement>(null)
  const stick = useRef(true)
  const follow = useCallback((pin = false) => {
    const el = ref.current
    if (pin) stick.current = true
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [])
  useLayoutEffect(() => {
    // A keyboard that opens makes the scroller shorter.
    const el = ref.current
    if (!el) return
    const resized = new ResizeObserver(() => follow())
    resized.observe(el)
    return () => resized.disconnect()
  }, [follow])
  return (
    <main
      class="scroll"
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 120
      }}
    >
      <Follow.Provider value={follow}>{children}</Follow.Provider>
    </main>
  )
}

function Turns({ id, s }: { id: string; s: S | undefined }) {
  const read = turnsOf(id).value
  const sent = sentTo(id).value
  const follow = useContext(Follow)
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
      loadTurns(id)
    }, 1000)
    return () => clearTimeout(again)
  }, [id, s, turns, sent])
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
            {turn.reply ? <Markdown class="reply" src={turn.reply} /> : !running && <p class="muted">No reply</p>}
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

const Running = ({ s }: { s: S }) => <p class={`running ${s.state}`}>{status(s)}…</p>

// PromptText is a prompt of a turn, or one the page sent that no turn shows
// yet.
function PromptText({ p, sent = false }: { p: Prompt; sent?: boolean }) {
  const [open, setOpen] = useState(false)
  const long = p.text.length > 600 || p.text.split('\n').length > 12
  const who = sent ? 'sent' : p.from === 'peer' ? 'from an agent' : p.from === 'task' ? 'background task' : p.from
  return (
    <div class={cls('prompt', p.from && 'from', sent && 'sent', long && !open && 'folded')}>
      {who && <span class="who">{who}</span>}
      {p.text || '(no text)'}
      {long && (
        <button class="more" type="button" onClick={() => setOpen(!open)}>
          {open ? 'Show less' : 'Show all'}
        </button>
      )}
    </div>
  )
}

function Screen({ id }: { id: string }) {
  const read = screenOf(id).value
  const follow = useContext(Follow)
  // biome-ignore lint/correctness/useExhaustiveDependencies: the screen read changes what shows
  useLayoutEffect(follow, [follow, read])
  const text = read.value ?? (read.error ? explain(read.error) : 'Reading its screen…')
  return <pre class="terminal">{text}</pre>
}

function Composer({ id, s, screen, say }: { id: string; s: S | undefined; screen: boolean; say: Say }) {
  const draft = `tmag.draft.${id}`
  const [text, setText] = useState(() => sessionStorage.getItem(draft) ?? '')
  const [busy, setBusy] = useState(false)
  const box = useRef<HTMLTextAreaElement>(null)

  // The box grows with what it holds, up to 40% of the window.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the text sets the box's scrollHeight
  useLayoutEffect(() => {
    const el = box.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, innerHeight * 0.4)}px`
  }, [text])

  const edit = (value: string) => {
    setText(value)
    if (value) sessionStorage.setItem(draft, value)
    else sessionStorage.removeItem(draft)
  }
  const prompts = !s || can(s, 'send')
  // A prompt is told from the same text sent before by what the turns held
  // as it went: it waits for the first read of them.
  const read = turnsOf(id).value
  const sendable = !busy && !!s && prompts && !!text.trim() && !!(read.value || read.error)

  async function run(verb: 'send' | 'interrupt') {
    setBusy(true)
    try {
      if (verb === 'send') {
        await act({ verb, target: id, text })
        edit('')
        // It shows at the end of the turns, if they show and it starts one.
        if (screen || !follows(text)) say('Sent')
      } else {
        await act({ verb, target: id })
        say('Interrupt sent')
      }
    } catch (e) {
      say(outcome(e, verb), true)
    }
    setBusy(false)
  }

  return (
    <form
      class="composer"
      onSubmit={(e) => {
        e.preventDefault()
        if (sendable) void run('send')
      }}
    >
      {s && stoppable(s) && (
        <button class="stop" type="button" disabled={busy} onClick={() => run('interrupt')}>
          Stop
        </button>
      )}
      {prompts ? (
        <>
          <textarea
            ref={box}
            rows={1}
            aria-label="Message"
            placeholder="Message"
            value={text}
            onInput={(e) => edit(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && sendable) {
                e.preventDefault()
                void run('send')
              }
            }}
          />
          <button class="primary" type="submit" disabled={!sendable}>
            Send
          </button>
        </>
      ) : (
        <p class="muted">tmag has no way to prompt this session.</p>
      )}
    </form>
  )
}

// useToast holds a short message: a few seconds, longer for an error.
function useToast(): [{ text: string; error: boolean }, Say] {
  const [toast, setToast] = useState({ text: '', error: false })
  const hide = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(hide.current), [])
  const say: Say = (text, error = false) => {
    clearTimeout(hide.current)
    setToast({ text, error })
    hide.current = setTimeout(() => setToast({ text: '', error: false }), error ? 12_000 : 3_000)
  }
  return [toast, say]
}
