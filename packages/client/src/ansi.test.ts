import { describe, expect, it } from 'vitest'
import { runs } from './ansi'

// What a pane shows, as capture-pane -e prints it: the text, in runs that
// share a style, and nothing an escape could make of it.
describe('runs', () => {
  it('reads colours, attributes and their resets', () => {
    const [line] = runs('\x1b[1;31mred\x1b[0m plain \x1b[38;5;208morange\x1b[39m \x1b[48;2;1;2;255;7mblue\x1b[0m')
    expect(line).toEqual([
      { text: 'red', style: { bold: true, fg: 1 } },
      { text: ' plain ', style: {} },
      { text: 'orange', style: { fg: 208 } },
      { text: ' ', style: {} },
      { text: 'blue', style: { bg: '#0102ff', reverse: true } },
    ])
  })
  it('carries a style to the next line, as a terminal does', () => {
    expect(runs('\x1b[7m❯ 1. Yes\n  2. No\x1b[27m end')).toEqual([
      [{ text: '❯ 1. Yes', style: { reverse: true } }],
      [
        { text: '  2. No', style: { reverse: true } },
        { text: ' end', style: {} },
      ],
    ])
  })
  it('drops every other escape and control character', () => {
    expect(runs('a\x1b]8;;https://x.example\x07link\x1b]8;;\x07\x1b[2Kb\x1b(B\x07c\x1b[?25l')).toEqual([
      [{ text: 'alinkbc', style: {} }],
    ])
    expect(runs('<img src=x onerror=alert(1)>')).toEqual([[{ text: '<img src=x onerror=alert(1)>', style: {} }]])
  })
  it('stops at a colour it cannot read', () => {
    expect(runs('\x1b[38;5mx')).toEqual([[{ text: 'x', style: {} }]])
  })
})
