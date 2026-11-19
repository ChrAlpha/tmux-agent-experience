import { useState } from 'preact/hooks'
import type { Computer } from '../computer'
import { minimum } from '../contract'
import { hash } from '../route'
import { computers, forget, title } from '../store'
import { cls, explain } from './explain'
import { useFrame } from './frame'
import { Header } from './Header'

// Settings are the computers this device works with: how each answers, the
// credential the device holds there, and the way to forget it; and the way
// to add another.
export function Settings() {
  const frame = useFrame()
  return (
    <div class="screen settings" ref={frame}>
      <Header title="Computers" />
      <main class="scroll">
        <ul class="computers">
          {computers.value.map((c) => (
            <Machine key={c.name} c={c} />
          ))}
        </ul>
        <a class="button" href={hash({ name: 'add' })}>
          Add a computer
        </a>
        <p class="muted about">tmag-client {import.meta.env.VERSION}</p>
      </main>
    </div>
  )
}

// state says how a computer answers.
function state(c: Computer): { text: string; tone: '' | 'wait' | 'bad' } {
  const p = c.problem.value
  if (c.live.value && c.outdated.value)
    return {
      text: `tmag ${c.outdated.value} is older than this page needs: update it to ${minimum} or later.`,
      tone: 'bad',
    }
  if (c.live.value) return { text: `Connected · tmag ${c.info.value?.version ?? ''}`.trim(), tone: '' }
  if (p) return { text: `${explain(p)} Trying again…`, tone: 'bad' }
  return { text: 'Connecting…', tone: 'wait' }
}

// Machine is one computer: forgetting it drops the credential from this
// device, which the person then revokes on the computer.
function Machine({ c }: { c: Computer }) {
  const [asking, setAsking] = useState(false)
  const s = state(c)
  const device = c.credential
  return (
    <li class="computer">
      <div class="line1">
        <span class={cls('light', s.tone)} aria-hidden="true" />
        <span class="name">{title(c)}</span>
        <span class="meta mono">{c.name ? `/${c.name}/` : location.host}</span>
      </div>
      <p class={cls('state', s.tone === 'bad' && 'error')}>{s.text}</p>
      <p class="muted">
        This device: {device.label} · <span class="mono">{device.id}</span>
      </p>
      {asking ? (
        <div class="confirm">
          <p>
            The page forgets {title(c)} and its credential. Revoke it on that computer with{' '}
            <code>tmag auth revoke {device.id}</code>
          </p>
          <div class="actions">
            <button type="button" onClick={() => setAsking(false)}>
              Keep
            </button>
            <button class="danger" type="button" onClick={() => forget(c)}>
              Forget
            </button>
          </div>
        </div>
      ) : (
        <button class="link danger" type="button" onClick={() => setAsking(true)}>
          Forget this computer
        </button>
      )}
    </li>
  )
}
