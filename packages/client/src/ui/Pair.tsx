import { useEffect, useState } from 'preact/hooks'
import { Failure } from '../api'
import { nameable, parse } from '../route'
import { add, back, go, linked, named, notice } from '../store'
import { explain } from './explain'
import { useFrame } from './frame'
import { Header } from './Header'
import { useVisit } from './visit'

// Pair pairs the page with a computer: the one that serves it, or another
// that the server in front of it proxies at `/NAME/v2/`.
export function Pair({ adding = false }: { adding?: boolean }) {
  const [code, setCode] = useState('')
  const [name, setName] = useState(adding ? (linked.value?.name ?? '') : '')
  const [other, setOther] = useState(adding && (!!named('') || !!linked.value?.name))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const at = other ? name.trim().toLowerCase() : ''
  // Where a link to the computer being added went belongs to this visit,
  // and so does going on once it is paired: a pairing that ends after the
  // person left keeps its credential and moves them nowhere.
  const visit = useVisit()
  const frame = useFrame()
  useEffect(
    () => () => {
      linked.value = null
    },
    [],
  )

  async function submit(ev: Event) {
    ev.preventDefault()
    if (other && !nameable(at)) {
      setError(
        'A name is lowercase letters, digits and -, at most 32, and none of the words links use: s, r, p, start, add, projects, settings.',
      )
      return
    }
    setBusy(true)
    setError('')
    const then = linked.value
    const v = visit()
    try {
      await add(at, code.replace(/\s+/g, '').toUpperCase())
      if (!v.here()) {
        if (v.shown()) setBusy(false)
        return
      }
      if (then?.name === at) go(parse(then.hash), { replace: true })
      else if (adding) back()
    } catch (e) {
      if (!v.shown()) return
      setError(
        e instanceof Failure && e.code === 'pairing'
          ? 'That code is wrong, used or expired: run tmag auth pair again.'
          : explain(e),
      )
      setBusy(false)
    }
  }

  return (
    <div class="screen pair" ref={frame}>
      <Header title={adding ? 'Add a computer' : 'Pair this device'} up={adding} />
      <main class="scroll">
        {notice.value && <p class="notice">{notice.value}</p>}
        {!adding && (
          <p class="lead">
            See what the coding agents on your computer do, answer what they ask, and prompt them, from here.
          </p>
        )}
        <ol class="steps">
          <li>
            On the computer, run <pre>tmag auth pair phone</pre>
          </li>
          <li>Enter the code it prints. A code works once, for ten minutes.</li>
        </ol>
        <form onSubmit={submit}>
          <label class="check">
            <input
              type="checkbox"
              checked={other}
              disabled={adding && !!named('')}
              onChange={(e) => setOther(e.currentTarget.checked)}
            />
            Another computer, which this page reaches at /name/
          </label>
          {other && (
            <input
              name="computer"
              aria-label="Name"
              placeholder="Name"
              autocapitalize="none"
              spellcheck={false}
              required
              value={name}
              onInput={(e) => setName(e.currentTarget.value)}
            />
          )}
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
