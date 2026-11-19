import { describe, expect, it } from 'vitest'
import { hash, nameable, parse, type Route } from './route'

describe('routes', () => {
  // A link names a computer by its name, the one that serves the page by
  // none, and reads back as it was written.
  it('writes and reads every route', () => {
    const routes: Route[] = [
      { name: 'inbox' },
      { name: 'projects' },
      { name: 'settings' },
      { name: 'add' },
      { name: 'session', at: '', id: '0a000000-0000-7000-8000-00000000000a' },
      { name: 'session', at: 'mini', id: 'a/b c', request: 'req 1' },
      { name: 'start', at: '' },
      { name: 'start', at: 'mini' },
      { name: 'start', at: '', dir: '/home/me/src/api' },
      { name: 'start', at: 'mini', dir: '/a b/c#d' },
      { name: 'pane', at: '', pane: '%12' },
      { name: 'pane', at: 'mini', pane: '/tmp/tmux-1000/default:%3' },
    ]
    for (const r of routes) expect(parse(hash(r)), hash(r)).toEqual(r)
    expect(hash({ name: 'session', at: 'mini', id: 'x', request: 'r1' })).toBe('#/mini/s/x/r/r1')
    expect(hash({ name: 'start', at: '', dir: '/src/api' })).toBe('#/start/%2Fsrc%2Fapi')
  })

  // The links tmag-notify sends, and those the page wrote before it had
  // projects, open what they named.
  it('reads the links it gave before', () => {
    expect(parse('#/s/abc')).toEqual({ name: 'session', at: '', id: 'abc' })
    expect(parse('#/mini/s/abc/r/r9')).toEqual({ name: 'session', at: 'mini', id: 'abc', request: 'r9' })
    expect(parse('#/mini/start')).toEqual({ name: 'start', at: 'mini' })
    expect(parse('#/p/%2512')).toEqual({ name: 'pane', at: '', pane: '%12' })
  })

  // A computer named projects or settings, as one paired before they were
  // screens may be, keeps its links.
  it('reads a computer named by a word a screen alone uses', () => {
    expect(parse('#/projects/s/abc')).toEqual({ name: 'session', at: 'projects', id: 'abc' })
    expect(parse(hash({ name: 'start', at: 'settings', dir: '/x' }))).toEqual({
      name: 'start',
      at: 'settings',
      dir: '/x',
    })
    expect(parse('#/projects')).toEqual({ name: 'projects' })
  })

  it('reads anything else as the inbox', () => {
    for (const h of [
      '',
      '#',
      '#/',
      '#/s',
      '#/mini',
      '#/s/x/r',
      '#/s/x/r/y/z',
      '#/s/%E0%A4%A',
      '#/Mini/s/x',
      '#/start/x',
      '#/start/%2Fa/b',
      '#/p',
      '#/p/%251/x',
      '#/projects/x',
      '#/s/s/x',
    ])
      expect(parse(h), h).toEqual({ name: 'inbox' })
  })

  it('names no computer by a word a route uses', () => {
    for (const n of ['s', 'r', 'p', 'start', 'add', 'projects', 'settings', '', 'Mini', 'a_b', 'x'.repeat(33)])
      expect(nameable(n), n).toBe(false)
    for (const n of ['mini', 'box-2', 'x'.repeat(32)]) expect(nameable(n), n).toBe(true)
  })
})
