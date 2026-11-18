import { useState } from 'preact/hooks'
import { Failure, pair } from '../api'
import { notice, paired } from '../store'
import { explain } from './explain'

export function Pair() {
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(ev: Event) {
    ev.preventDefault()
    setBusy(true)
    setError('')
    try {
      paired(await pair(code.replace(/\s+/g, '').toUpperCase()))
    } catch (e) {
      setError(
        e instanceof Failure && e.code === 'pairing'
          ? 'That code is wrong, used or expired: run tmag auth pair again.'
          : explain(e),
      )
      setBusy(false)
    }
  }

  return (
    <div class="screen pair">
      <header>
        <h1>Pair this device</h1>
      </header>
      <main class="scroll">
        {notice.value && <p class="notice">{notice.value}</p>}
        <p>On the computer, run</p>
        <pre>tmag auth pair phone</pre>
        <p>and enter the code it prints. A code works once, for ten minutes.</p>
        <form onSubmit={submit}>
          <input
            name="code"
            aria-label="Code"
            placeholder="Code"
            autocomplete="one-time-code"
            autocapitalize="characters"
            spellcheck={false}
            required
            value={code}
            onInput={(e) => setCode(e.currentTarget.value)}
          />
          <button class="primary" type="submit" disabled={busy}>
            Pair
          </button>
        </form>
        {error && (
          <p class="error" role="alert">
            {error}
          </p>
        )}
      </main>
    </div>
  )
}
