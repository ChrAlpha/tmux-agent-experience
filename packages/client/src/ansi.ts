// A terminal's lines with their SGR escapes, as `read --lines --json` gives
// them in `ansi`, read into runs of text that share a style. Every other
// escape a capture may hold is dropped; the text stays text.

/** How a run of text looks: tmux's colours by number, or by RGB. */
export interface Style {
  fg?: Colour
  bg?: Colour
  bold?: boolean
  dim?: boolean
  italic?: boolean
  underline?: boolean
  reverse?: boolean
}

/** One of the 256 colours, or a true colour as #rrggbb. */
export type Colour = number | string

export interface Run {
  text: string
  style: Style
}

const hex = (n: number) => n.toString(16).padStart(2, '0')

// sgr applies one Select Graphic Rendition sequence's parameters to a style.
function sgr(style: Style, params: number[]): Style {
  let s = { ...style }
  for (let i = 0; i < params.length; i++) {
    const p = params[i] ?? 0
    if (p === 0) s = {}
    else if (p === 1) s.bold = true
    else if (p === 2) s.dim = true
    else if (p === 3) s.italic = true
    else if (p === 4) s.underline = true
    else if (p === 7) s.reverse = true
    else if (p === 22) s.bold = s.dim = undefined
    else if (p === 23) s.italic = undefined
    else if (p === 24) s.underline = undefined
    else if (p === 27) s.reverse = undefined
    else if (p >= 30 && p <= 37) s.fg = p - 30
    else if (p >= 90 && p <= 97) s.fg = p - 90 + 8
    else if (p === 39) s.fg = undefined
    else if (p >= 40 && p <= 47) s.bg = p - 40
    else if (p >= 100 && p <= 107) s.bg = p - 100 + 8
    else if (p === 49) s.bg = undefined
    else if (p === 38 || p === 48) {
      const which = p === 38 ? 'fg' : 'bg'
      if (params[i + 1] === 5 && i + 2 < params.length) {
        s[which] = Math.min(255, params[i + 2] ?? 0)
        i += 2
      } else if (params[i + 1] === 2 && i + 4 < params.length) {
        const [r, g, b] = params.slice(i + 2, i + 5).map((c) => Math.min(255, c))
        s[which] = `#${hex(r ?? 0)}${hex(g ?? 0)}${hex(b ?? 0)}`
        i += 4
      } else break
    }
  }
  return Object.fromEntries(Object.entries(s).filter(([, v]) => v !== undefined)) as Style
}

// The escapes a capture may hold: CSI sequences (SGR among them), OSC
// sequences ended by BEL or ST, character set designations, and two-character
// escapes.
// biome-ignore lint/suspicious/noControlCharactersInRegex: escapes are what it reads
const sequences = /\x1b(?:\[([0-?]*)[ -/]*([@-~])|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()*+].|[@-Z\\-_])/g

// runs reads lines with SGR escapes into runs of text, one array a line; a
// style carries from one line to the next, as in a terminal.
export function runs(ansi: string): Run[][] {
  const lines: Run[][] = []
  let style: Style = {}
  for (const line of ansi.split('\n')) {
    const out: Run[] = []
    let at = 0
    const push = (text: string) => {
      // biome-ignore lint/suspicious/noControlCharactersInRegex: control characters are what it drops
      const clean = text.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '')
      if (!clean) return
      const last = out[out.length - 1]
      if (last && last.style === style) last.text += clean
      else out.push({ text: clean, style })
    }
    for (const m of line.matchAll(sequences)) {
      push(line.slice(at, m.index))
      at = m.index + m[0].length
      if (m[2] === 'm' && /^[0-9;:]*$/.test(m[1] ?? ''))
        style = sgr(
          style,
          (m[1] || '0').split(/[;:]/).map((n) => Number(n) || 0),
        )
    }
    push(line.slice(at))
    lines.push(out)
  }
  return lines
}
