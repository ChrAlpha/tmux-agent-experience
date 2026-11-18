import type { ComponentChildren } from 'preact'
import { useState } from 'preact/hooks'
import { Failure } from '../api'
import type { Computer } from '../computer'
import { type Action, type StartDoc, startable } from '../contract'
import { hash, type Route } from '../route'
import { back, computers, later, named, title } from '../store'
import { sessions } from '../view'
import { explain, outcome, refusal } from './explain'
import { useVisit } from './visit'

const another = '\u0000another'

// Start starts an agent on a computer, in a window of its own beside tmag
// serve's, in a directory its sessions run in or another, with a template
// the person set there or by its name, and gives it a prompt as send does.
// It starts on the computer the person picks, else the one its link names,
// once that one said it starts agents, else the first that does.
export function Start({ at }: { at: string }) {
  const all = computers.value
  const able = all.filter((x) => x.can('start'))
  const routed = named(at)
  const [picked, setPicked] = useState<string | null>(null)
  const name = picked ?? (routed?.can('start') ? routed.name : routed && !routed.info.value ? null : able[0]?.name)
  const c = able.find((x) => x.name === name)
  const dirs = c ? [...new Set(sessions(c.view.value).flatMap((s) => (s.cwd ? [s.cwd] : [])))].sort() : []
  const [who, setWho] = useState<string>('claude')
  const [template, setTemplate] = useState('')
  // The directory chosen, if the person chose one; else its sessions' first.
  const [dir, setDir] = useState<string | null>(null)
  const [typed, setTyped] = useState('')
  const [worktree, setWorktree] = useState('')
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [started, setStarted] = useState<{ at: string; id: string } | null>(null)
  // A start can take a while: what it ends in goes to the sheet while the
  // person stays on it, and to a bar once they left it.
  const visit = useVisit()

  if (!c) {
    const waits = name === null ? routed : all.find((x) => !x.info.value)
    return (
      <Sheet>
        <p class="empty">
          {waits
            ? `Asking ${title(waits)} what it serves…${waits.problem.value ? ` ${explain(waits.problem.value)}` : ''}`
            : 'No computer the page works with starts agents: its tmag is older than tmag start.'}
        </p>
      </Sheet>
    )
  }
  const chosen = dir === null ? (dirs[0] ?? another) : dirs.includes(dir) ? dir : another
  const where = chosen === another ? typed.trim() : chosen

  async function submit(ev: Event) {
    ev.preventDefault()
    if (!c) return
    const problem = !where.startsWith('/')
      ? 'Give the directory as an absolute path.'
      : who === 'template' && !/^[a-z0-9_-]{1,32}$/.test(template)
        ? 'A template is named by lowercase letters, digits, - and _, as @tmag-start-NAME names it.'
        : ''
    if (problem) return setError(problem)
    const prompt = text.trim() ? text : ''
    const action: Action = {
      verb: 'start',
      ...(who === 'template' ? { template } : { agent: who as (typeof startable)[number] }),
      dir: where,
      ...(worktree.trim() ? { worktree: worktree.trim() } : {}),
      ...(prompt ? { text: prompt } : {}),
    }
    const what = `${who === 'template' ? `@tmag-start-${template}` : who} in ${where}${all.length > 1 ? ` on ${title(c)}` : ''}`
    setBusy(true)
    setError('')
    setStarted(null)
    const v = visit()
    // left says what became of the start in a bar, once the person left
    // where they began it; a sheet still showing is ready for another.
    const left = (text: string, go?: Route, label?: string) => {
      later.value = { text: `${what}: ${text}`, ...(go && { href: hash(go), label }) }
      if (v.shown()) setBusy(false)
    }
    const ended = (e: unknown) => {
      const r = after(c, e, prompt)
      if (!v.here()) left(r.text, r.go, r.label)
      else if (r.go?.name === 'pane') location.replace(hash(r.go))
      else {
        setError(r.text)
        setStarted(r.go?.name === 'session' ? { at: c.name, id: r.go.id } : null)
        setBusy(false)
      }
    }
    try {
      const doc = await c.act<StartDoc>(action)
      if (!doc?.session?.id) return ended(new Failure(200, 'network', 'its answer was cut short'))
      const go: Route = { name: 'session', at: c.name, id: doc.session.id }
      if (v.here()) location.replace(hash(go))
      else left('it started.', go, 'Open it')
    } catch (e) {
      ended(e)
    }
  }

  return (
    <Sheet>
      <form class="start" onSubmit={submit}>
        {all.length > 1 && (
          <label>
            Computer
            <select
              value={c.name}
              onChange={(e) => {
                setPicked(e.currentTarget.value)
                setDir(null)
              }}
            >
              {able.map((x) => (
                <option key={x.name} value={x.name}>
                  {title(x)}
                </option>
              ))}
            </select>
          </label>
        )}
        <fieldset>
          <legend>Agent</legend>
          {[...startable, 'template'].map((a) => (
            <label key={a} class="choice">
              <input type="radio" name="who" value={a} checked={who === a} onChange={() => setWho(a)} />
              {a === 'template' ? 'A template' : a}
            </label>
          ))}
          {who === 'template' && (
            <input
              class="field"
              aria-label="Template"
              placeholder="Name, as @tmag-start-NAME"
              autocapitalize="none"
              spellcheck={false}
              value={template}
              onInput={(e) => setTemplate(e.currentTarget.value)}
            />
          )}
        </fieldset>
        <fieldset>
          <legend>Directory</legend>
          {dirs.length > 0 && (
            <select aria-label="Directory" value={chosen} onChange={(e) => setDir(e.currentTarget.value)}>
              {dirs.map((d) => (
                <option key={d} value={d}>
                  {d}
                </option>
              ))}
              <option value={another}>Another directory…</option>
            </select>
          )}
          {chosen === another && (
            <input
              class="field"
              aria-label="Path"
              placeholder="/an/absolute/path"
              autocapitalize="none"
              spellcheck={false}
              value={typed}
              onInput={(e) => setTyped(e.currentTarget.value)}
            />
          )}
        </fieldset>
        <label>
          Worktree
          <input
            class="field"
            aria-label="Worktree"
            placeholder="A branch, to work on it in a worktree of its own (optional)"
            autocapitalize="none"
            spellcheck={false}
            value={worktree}
            onInput={(e) => setWorktree(e.currentTarget.value)}
          />
        </label>
        <label>
          Prompt
          <textarea
            class="field"
            aria-label="Prompt"
            rows={4}
            placeholder="What to work on (optional)"
            value={text}
            onInput={(e) => setText(e.currentTarget.value)}
          />
        </label>
        <button class="primary" type="submit" disabled={busy}>
          {busy ? 'Starting…' : 'Start'}
        </button>
        {error && (
          <p class="error" role="alert">
            {error}
            {started && (
              <>
                {' '}
                <a href={hash({ name: 'session', ...started })}>Open it</a>
              </>
            )}
          </p>
        )}
      </form>
    </Sheet>
  )
}

function Sheet({ children }: { children: ComponentChildren }) {
  return (
    <div class="screen sheet">
      <header>
        <button class="back" type="button" aria-label="Agents" onClick={back}>
          ‹
        </button>
        <h1>Start an agent</h1>
      </header>
      <main class="scroll">{children}</main>
    </div>
  )
}

// after says what a start that failed ended in, and where to see it: the
// pane of an agent that shows no session yet, which holds its prompt; the
// session of one that started and whose prompt then failed, whose message
// holds the prompt when tmag refused to give it, and only then, since any
// other failure may follow its delivery.
function after(c: Computer, e: unknown, prompt: string): { text: string; go?: Route; label?: string } {
  const doc = e instanceof Failure ? e.doc : undefined
  if (doc?.pane) {
    const pane = `${doc.pane.server}:${doc.pane.id}`
    if (prompt) c.draftOf(pane).value = prompt
    return {
      text: 'it shows no session yet, and may ask something first.',
      go: { name: 'pane', at: c.name, pane },
      label: 'Open its pane',
    }
  }
  if (doc?.started) {
    const go: Route = { name: 'session', at: c.name, id: doc.started.id }
    const refused = refusal(e, 'send')
    if (!prompt) return { text: `It started. tmag says: ${explain(e)}`, go, label: 'Open it' }
    if (refused === undefined) {
      return {
        text: `It started, and whether it got the prompt is unknown: look at it before sending the prompt again. tmag says: ${explain(e)}`,
        go,
        label: 'Open it',
      }
    }
    const into = c.draftOf(doc.started.id)
    into.value = [into.value.trimEnd(), prompt].filter(Boolean).join('\n\n')
    return {
      text: `It started, but tmag did not give it the prompt, which waits in its message: ${refused}`,
      go,
      label: 'Open it',
    }
  }
  return { text: outcome(e, 'start') }
}
