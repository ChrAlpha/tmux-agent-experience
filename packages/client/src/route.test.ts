import { describe, expect, it } from 'vitest'
import { hash, nameable, parse, type Route } from './route'

describe('routes', () => {
  // A link names a computer by its name, the one that serves the page by
  // none, and reads back as it was written.
  it('writes and reads every route', () => {
    const routes: Route[] = [
      { name: 'list' },
      { name: 'add' },
      { name: 'session', at: '', id: '0a000000-0000-7000-8000-00000000000a' },
      { name: 'session', at: 'mini', id: 'a/b c', request: 'req 1' },
    ]
    for (const r of routes) expect(parse(hash(r)), hash(r)).toEqual(r)
    expect(hash({ name: 'session', at: 'mini', id: 'x', request: 'r1' })).toBe('#/mini/s/x/r/r1')
  })

  it('reads anything else as the list', () => {
    for (const h of ['', '#', '#/', '#/s', '#/mini', '#/s/x/r', '#/s/x/r/y/z', '#/s/%E0%A4%A', '#/Mini/s/x', '#/start'])
      expect(parse(h), h).toEqual({ name: 'list' })
  })

  it('names no computer by a word a route uses', () => {
    for (const n of ['s', 'r', 'p', 'start', 'add', '', 'Mini', 'a_b', 'x'.repeat(33)])
      expect(nameable(n), n).toBe(false)
    for (const n of ['mini', 'box-2', 'x'.repeat(32)]) expect(nameable(n), n).toBe(true)
  })
})
