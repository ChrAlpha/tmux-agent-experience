import type { Pane, Session } from '../contract'
import { credential, forget, loaded, now, view } from '../store'
import { ago, helpers, paneOf, place, sessions, status } from '../view'

export function Agents() {
  const v = view.value
  const all = sessions(v)
  const device = credential.value
  return (
    <div class="screen list">
      <header>
        <h1>Agents</h1>
      </header>
      <main class="scroll">
        <ul class="sessions">
          {loaded.value && !all.length && (
            <li class="empty">No agent is running. Start one in tmux, or ask a running one to start a peer.</li>
          )}
          {all.map((s) => (
            <Row key={`${s.agent}/${s.id}`} s={s} pane={paneOf(v, s)} />
          ))}
        </ul>
        {device && (
          <footer>
            <span>
              This device: {device.label} · {device.id}
            </span>
            <button
              class="link"
              type="button"
              onClick={() => {
                if (
                  confirm(
                    `Forget this device? Revoke its credential on the computer with: tmag auth revoke ${device.id}`,
                  )
                )
                  forget()
              }}
            >
              Forget
            </button>
          </footer>
        )}
      </main>
    </div>
  )
}

function Row({ s, pane }: { s: Session; pane: Pane | undefined }) {
  const asks = s.state === 'blocked' ? s.requests[0]?.tool : undefined
  return (
    <li>
      <a class={`row ${s.state}`} href={`#/s/${encodeURIComponent(s.id)}`}>
        <span class="dot" />
        <span class="what">
          <span class="name">{place(s)}</span>
          <span class="meta">{[s.agent, status(s), helpers(s), asks, pane?.target].filter(Boolean).join(' · ')}</span>
        </span>
        <span class="when">{ago(s.since, now.value)}</span>
      </a>
    </li>
  )
}
