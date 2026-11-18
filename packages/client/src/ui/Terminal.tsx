import { Fragment } from 'preact'
import { useMemo } from 'preact/hooks'
import { type Colour, type Run, runs } from '../ansi'
import { cls } from './explain'

// The 6 levels of each primary in xterm's 256-colour cube.
const level = [0, 95, 135, 175, 215, 255]

// css is a colour as CSS has it: the 16 a theme sets by name, the rest of
// the 256 by xterm's rule, a true colour as it came.
function css(c: Colour): string {
  if (typeof c === 'string') return c
  if (c < 16) return `var(--t${c})`
  if (c < 232) {
    const i = c - 16
    return `rgb(${level[Math.floor(i / 36)]}, ${level[Math.floor(i / 6) % 6]}, ${level[i % 6]})`
  }
  const g = 8 + 10 * (c - 232)
  return `rgb(${g}, ${g}, ${g})`
}

// Span is a run of text in its style. Its colours are set as properties,
// which the page's Content-Security-Policy allows where a style attribute
// it would not.
function Span({ r }: { r: Run }) {
  const s = r.style
  let fg = s.fg === undefined ? undefined : css(s.fg)
  let bg = s.bg === undefined ? undefined : css(s.bg)
  if (s.reverse) [fg, bg] = [bg ?? 'var(--term-bg)', fg ?? 'var(--term-fg)']
  const plain = !fg && !bg && !s.bold && !s.dim && !s.italic && !s.underline
  if (plain) return <>{r.text}</>
  return (
    <span
      class={cls(s.bold && 'b', s.dim && 'd', s.italic && 'i', s.underline && 'u')}
      style={{ color: fg, backgroundColor: bg }}
    >
      {r.text}
    </span>
  )
}

// Terminal shows what a pane shows: in its colours where the computer gave
// them, else as plain text.
export function Terminal({ text, ansi }: { text: string; ansi?: string }) {
  const lines = useMemo(() => (ansi === undefined ? null : runs(ansi)), [ansi])
  if (!lines) return <pre class="terminal">{text}</pre>
  return (
    <pre class="terminal">
      {lines.map((line, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a screen's lines are its rows
        <Fragment key={i}>
          {line.map((r, j) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: a line's runs are its columns
            <Span key={j} r={r} />
          ))}
          {i < lines.length - 1 && '\n'}
        </Fragment>
      ))}
    </pre>
  )
}
