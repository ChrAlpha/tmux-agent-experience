import type { Computer } from '../computer'
import type { Pane } from '../contract'
import { hash } from '../route'
import { computers, type Entry, forget, inbox, now, title } from '../store'
import { ago, done, helpers, paneOf, place, status, ties } from '../view'
import { cls } from './explain'

export function Agents() {
  const all = inbox.value
  const several = computers.value.length > 1
  const loaded = computers.value.every((c) => c.loaded.value)
  return (
    <div class="screen list">
      <header>
        <h1>Agents</h1>
        {computers.value.some((c) => c.can('start')) && (
          <a class="mode" href={hash({ name: 'start', at: '' })}>
            Start
          </a>
        )}
      </header>
      <main class="scroll">
        <ul class="sessions">
          {loaded && !all.length && (
            <li class="empty">No agent is running. Start one in tmux, or ask a running one to start a peer.</li>
          )}
          {all.map((e) => (
            <Row key={`${e.c.name}/${e.s.agent}/${e.s.id}`} e={e} several={several} />
          ))}
        </ul>
        <footer>
          {several && <span>This device:</span>}
          {computers.value.map((c) => (
            <Device key={c.name} c={c} several={several} />
          ))}
          <a class="link" href={hash({ name: 'add' })}>
            Add a computer
          </a>
        </footer>
      </main>
    </div>
  )
}

function Row({ e: { c, s }, several }: { e: Entry; several: boolean }) {
  const v = c.view.value
  const pane: Pane | undefined = paneOf(v, s)
  const asks = s.state === 'blocked' ? s.requests[0]?.tool : undefined
  const seen = c.seen.value
  const unseen = !!seen && done(s, seen)
  const tie = ties(v, s)[0]
  return (
    <li>
      <a class={cls('row', s.state, unseen && 'done')} href={hash({ name: 'session', at: c.name, id: s.id })}>
        <span class="dot" />
        <span class="what">
          <span class="name">
            {place(s)}
            {several && <small> · {title(c)}</small>}
          </span>
          <span class="meta">
            {[
              s.agent,
              unseen ? 'done' : status(s),
              tie && `${tie.text} ${place(tie.s)}`,
              helpers(s),
              asks,
              pane?.target,
            ]
              .filter(Boolean)
              .join(' · ')}
          </span>
        </span>
        <span class="when">{ago(s.since, now.value)}</span>
      </a>
    </li>
  )
}

// Device is this device's credential on a computer, which the page can
// forget; the computer is then the person's to tell to revoke it.
function Device({ c, several }: { c: Computer; several: boolean }) {
  const device = c.credential
  return (
    <div class="device">
      <span>
        {several ? `${title(c)}: ` : 'This device: '}
        {device.label} · {device.id}
      </span>
      <button
        class="link"
        type="button"
        onClick={() => {
          if (
            confirm(
              `Forget ${title(c)} on this device? Revoke its credential on that computer with: tmag auth revoke ${device.id}`,
            )
          )
            forget(c)
        }}
      >
        Forget
      </button>
    </div>
  )
}
