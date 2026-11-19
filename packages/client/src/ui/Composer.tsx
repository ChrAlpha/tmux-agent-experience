import { useLayoutEffect, useRef, useState } from 'preact/hooks'
import { Computer } from '../computer'
import type { Session as S } from '../contract'
import { holding, instanceOf } from '../store'
import { can, follows, stoppable } from '../view'
import { outcome } from './explain'
import { Send, Stop } from './icons'
import type { Say } from './toast'

// Composer is where the person prompts a session, and stops the turn it
// runs. What they write is a draft the tab keeps until it is sent.
export function Composer({
  c,
  id,
  s,
  screen,
  say,
}: {
  c: Computer
  id: string
  s: S | undefined
  screen: boolean
  say: Say
}) {
  const draft = c.draftOf(id)
  const text = draft.value
  const [mine, setBusy] = useState(false)
  // A prompt on its way from this screen or another holds the next one.
  const busy = mine || c.isSending(id)
  const box = useRef<HTMLTextAreaElement>(null)

  // The box grows with what it holds, up to 40% of the room under the
  // screen's bar, which a keyboard shrinks as it opens (see app.css).
  // biome-ignore lint/correctness/useExhaustiveDependencies: the text sets the box's scrollHeight
  useLayoutEffect(() => {
    const el = box.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight + el.offsetHeight - el.clientHeight}px`
  }, [text])

  const edit = (value: string) => {
    draft.value = value
  }
  const prompts = !s || (can(s, 'send') && c.can('send'))
  // A prompt is told from the same text sent before by what the turns held
  // as it went: it waits for the first read of them.
  const read = c.turnsOf(id).value
  const sendable = !busy && !!s && prompts && !!text.trim() && (!c.can('read') || !!(read.value || read.error))

  // A prompt leaves the box as it goes, and comes back to it if it did not,
  // before what was written meanwhile, at whichever address the page keeps
  // of the computer it went to; not to another computer that answers there
  // since: what is in a box is never what is on its way.
  async function run(verb: 'send' | 'interrupt') {
    setBusy(true)
    const instance = instanceOf(c)
    try {
      if (verb === 'send') {
        edit('')
        try {
          await c.act({ verb, target: id, text })
        } catch (e) {
          // A computer the page let go of meanwhile, as one that refused this
          // device, keeps it for the same instance, should it be paired again.
          const back = holding(instance, c) ?? (instance && instanceOf(c) === instance ? c : undefined)
          if (back) {
            const into = back.draftOf(id)
            into.value = into.value ? `${text}\n\n${into.value}` : text
          } else if (instance) Computer.keepFor(instance, id, text)
          throw e
        }
        // It shows at the end of the turns, if they show and it starts one.
        if (screen || !follows(text)) say('Sent')
      } else {
        await c.act({ verb, target: id })
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
      {s && stoppable(s) && c.can('interrupt') && (
        <button class="stop" type="button" aria-label="Stop" disabled={busy} onClick={() => run('interrupt')}>
          <Stop />
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
          <button class="send primary" type="submit" aria-label="Send" disabled={!sendable}>
            <Send />
          </button>
        </>
      ) : (
        <p class="muted">tmag has no way to prompt this session.</p>
      )}
    </form>
  )
}
