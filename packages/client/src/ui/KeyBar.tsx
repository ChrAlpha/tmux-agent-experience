import { useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Key } from '../contract'
import { outcome } from './explain'
import type { Say } from './toast'

// The keys a person answers an agent's dialog with, as the bar shows them.
const keys: { label: string; key: Key; name?: string }[] = [
  { label: 'Esc', key: 'Escape', name: 'Escape' },
  { label: 'Tab', key: 'Tab' },
  { label: '↑', key: 'Up', name: 'Up' },
  { label: '↓', key: 'Down', name: 'Down' },
  { label: '←', key: 'Left', name: 'Left' },
  { label: '→', key: 'Right', name: 'Right' },
  { label: '␣', key: 'Space', name: 'Space' },
  { label: '⌫', key: 'BSpace', name: 'Backspace' },
  { label: 'y', key: 'y' },
  { label: 'n', key: 'n' },
  { label: '1', key: '1' },
  { label: '2', key: '2' },
  { label: '3', key: '3' },
  { label: '^C', key: 'C-c', name: 'Control-C' },
]

// KeyBar types for the person into what a pane shows, as tmag keys does: the
// line in its field, if any, then the key pressed. Each goes with the screen
// the page shows, which tmag checks the pane still shows right before it
// types; until a read that began after it typed shows the screen, and while
// the screen could not be read again, the bar waits.
export function KeyBar({ c, target, say }: { c: Computer; target: string; say: Say }) {
  const read = c.screenOf(target).value
  const screen = read.value?.screen
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  // How many reads had begun when keys were last typed, or might have been.
  const [typed, setTyped] = useState(0)
  const waiting = busy || !!read.error || (read.began ?? 0) <= typed

  async function press(key: Key) {
    if (!screen || waiting) return
    setBusy(true)
    const line = text
    try {
      await c.act({ verb: 'keys', target, screen, keys: [key], ...(line ? { text: line } : {}) })
      // What the person wrote meanwhile stays for the next key.
      setText((now) => (now.startsWith(line) ? now.slice(line.length) : now))
    } catch (e) {
      say(outcome(e, 'keys'), true)
    }
    setTyped(c.begun)
    c.loadScreen(target)
    setBusy(false)
  }

  if (!screen) return null
  return (
    <div class="keybar">
      <div class="keys">
        {keys.map((k) => (
          <button
            key={k.key}
            type="button"
            aria-label={k.name ?? k.label}
            disabled={waiting}
            onClick={() => press(k.key)}
          >
            {k.label}
          </button>
        ))}
      </div>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void press('Enter')
        }}
      >
        <input
          class="line"
          aria-label="Type"
          placeholder="Type, then a key"
          autocapitalize="none"
          autocomplete="off"
          spellcheck={false}
          value={text}
          onInput={(e) => setText(e.currentTarget.value)}
        />
        <button class="primary enter" type="submit" aria-label="Enter" disabled={waiting}>
          ⏎
        </button>
      </form>
    </div>
  )
}
