import { useEffect } from 'preact/hooks'
import type { Computer } from '../computer'
import { hash } from '../route'
import { back, computers, title } from '../store'
import { cls } from './explain'
import { KeyBar } from './KeyBar'
import { Screen, Scroller, useToast } from './Session'

// PaneScreen is what a pane shows that no session of tmag's does yet, as an
// agent tmag started shows a trust dialog or a login, with keys to answer
// it. Once the pane shows a session, it links there, and the prompt the pane
// holds for it, one its start could not give, goes into that session's
// message for the person to send. The pane is a target as tmag takes it:
// server:%N, or %N on tmag serve's own server.
export function PaneScreen({ c, pane }: { c: Computer; pane: string }) {
  const [toast, say] = useToast()
  const at = pane.lastIndexOf(':')
  const server = at < 0 ? '' : pane.slice(0, at)
  const id = pane.slice(at + 1)
  const session = c.view.value.panes.find((p) => p.id === id && (!server || p.server === server) && p.session)?.session
  // What it serves is known once it said which computer it is.
  const known = !!c.info.value
  const reads = known && c.can('read')
  const held = c.draftOf(pane)
  // A pane changes with nothing to report: it is read every two seconds.
  useEffect(() => {
    if (!reads) return
    c.loadScreen(pane)
    const again = setInterval(() => document.hidden || c.loadScreen(pane), 2000)
    return () => clearInterval(again)
  }, [c, pane, reads])
  useEffect(() => {
    if (!session || !held.value) return
    const into = c.draftOf(session)
    into.value = [into.value.trimEnd(), held.value].filter(Boolean).join('\n\n')
    held.value = ''
  }, [c, session, held])
  return (
    <div class="screen session">
      <header>
        <button class="back" type="button" aria-label="Agents" onClick={back}>
          ‹
        </button>
        <div class="what">
          <h1 class="name">Pane {id}</h1>
          <div class="meta">
            {computers.value.length > 1 ? `${title(c)} · ` : ''}an agent tmag started, which shows no session yet
          </div>
          {session && (
            <div class="ties">
              <a href={hash({ name: 'session', at: c.name, id: session })}>Open its session</a>
            </div>
          )}
        </div>
      </header>
      {held.value && (
        <p class="held" role="status">
          Its prompt was not given: once the pane shows its session, it waits in that session's message.
        </p>
      )}
      <Scroller>
        {reads ? (
          <Screen c={c} target={pane} />
        ) : (
          <p class="empty">{known ? 'tmag on this computer does not read a pane.' : 'Reading its screen…'}</p>
        )}
      </Scroller>
      <p class={cls('toast', toast.error && 'error')} role="status">
        {toast.text}
      </p>
      {reads && c.can('keys') && <KeyBar c={c} target={pane} say={say} />}
    </div>
  )
}
