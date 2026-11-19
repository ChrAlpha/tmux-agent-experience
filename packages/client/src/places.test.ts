import { describe, expect, it } from 'vitest'
import { apart, expand, inside, snippet, tilde, where } from './places'

describe('where', () => {
  // A directory tmag names no repository for is a project of its own.
  it('names a directory by itself without a repository', () => {
    expect(where('/home/me/notes')).toEqual({ root: '/home/me/notes', name: 'notes', worktree: false, sub: '' })
  })

  // Every worktree of a repository, and every directory in one, is the
  // project of its main worktree.
  it('names a directory in a repository by its main worktree', () => {
    const repo = { top: '/src/api', main: '/src/api', branch: 'main' }
    expect(where('/src/api', repo)).toEqual({ root: '/src/api', name: 'api', branch: 'main', worktree: false, sub: '' })
    expect(where('/src/api/pkg/db', repo)).toMatchObject({ name: 'api', sub: 'pkg/db', worktree: false })
    const tree = { top: '/src/api/.worktrees/fix', main: '/src/api', branch: 'fix/login' }
    expect(where('/src/api/.worktrees/fix/web', tree)).toEqual({
      root: '/src/api',
      name: 'api',
      branch: 'fix/login',
      worktree: true,
      sub: 'web',
    })
    expect(where('/w/bare/x', { top: '/w/bare/x' })).toMatchObject({ root: '/w/bare/x', name: 'x', worktree: false })
  })
})

describe('paths', () => {
  it('writes and reads a path under the home directory from ~', () => {
    expect(tilde('/home/me/src', '/home/me')).toBe('~/src')
    expect(tilde('/home/me', '/home/me')).toBe('~')
    expect(tilde('/home/mel/src', '/home/me')).toBe('/home/mel/src')
    expect(tilde('/src', undefined)).toBe('/src')
    expect(expand('~/src', '/home/me')).toBe('/home/me/src')
    expect(expand('~', '/home/me/')).toBe('/home/me')
    expect(expand('~me/x', '/home/me')).toBe('~me/x')
    expect(inside('/a/b', '/a')).toBe('b')
    expect(inside('/ab', '/a')).toBeUndefined()
    expect(inside('/a', '/')).toBe('a')
  })

  it('tells apart paths that name alike by the fewest of their last directories', () => {
    expect(apart(['/work/customer-a/services/api', '/work/customer-b/services/api'])).toEqual([
      '…/customer-a/services/api',
      '…/customer-b/services/api',
    ])
    expect(apart(['~/work/api', '~/archive/api', '/srv/api'])).toEqual(['…/work/api', '…/archive/api', '/srv/api'])
    expect(apart(['/api', '/x/api'])).toEqual(['/api', '/x/api'])
    expect(apart(['/same/api', '/same/api'])).toEqual(['/same/api', '/same/api'])
    expect(apart(['/x/east/api', '/x/west/api', '/y/east/api'])).toEqual(['/x/east/api', '…/west/api', '/y/east/api'])
    expect(apart(['/one/api/customer-east-services', '/two/api/customer-west-services'])).toEqual([
      '…/customer-east-services',
      '…/customer-west-services',
    ])
  })

  it('shows a long end by where it parts from each other path, and its end', () => {
    expect(
      apart([
        '/work/customer-platform-integration-production-east/api',
        '/work/customer-platform-integration-production-west/api',
        '/work/dev/api',
      ]),
    ).toEqual(['…east/api', '…west/api', '…/dev/api'])
    expect(
      apart([
        '/work/customer-america-east/api',
        '/work/customer-america-west/api',
        '/work/customer-european-east/api',
        '/work/customer-european-west/api',
      ]),
    ).toEqual(['…america-east/api', '…america-west/api', '…european-east/api', '…european-west/api'])
    expect(apart(['/w/customerplatformintegrationeast/api', '/w/customerplatformintegrationwest/api'])).toEqual([
      '…grationeast/api',
      '…grationwest/api',
    ])
    const x = 'x'.repeat(60)
    expect(apart([`/work/customer-${x}a/api`, `/work/customer-${x}b/api`])).toEqual([
      '…xxxxxxxxa/api',
      '…xxxxxxxxb/api',
    ])
    expect(apart([`/w/a${x}east/api`, `/w/a${x}west/api`, `/w/b${x}east/api`, `/w/b${x}west/api`])).toEqual([
      '…a…xxeast/api',
      '…a…xxwest/api',
      '…b…xxeast/api',
      '…b…xxwest/api',
    ])
    expect(apart([`/${x}a/api`, `/${x}b/api`])).toEqual(['…xxxxxxxxa/api', '…xxxxxxxxb/api'])
    expect(apart([`/${x}/api`, `/w/${x}/api`])).toEqual([`/${x}/api`, `/w/${x}/api`])
    // Wide letters take two columns; a letter outside the basic plane is never cut in two.
    expect(apart([`/w/${'界'.repeat(17)}東/api`, `/w/${'界'.repeat(17)}西/api`])).toEqual([
      '…界界界界東/api',
      '…界界界界西/api',
    ])
    expect(apart(['/w/customerplatformintegration📁/api', '/w/customerplatformintegration📂/api'])).toEqual([
      '…egration📁/api',
      '…egration📂/api',
    ])
  })

  it('shows the few paths a computer names alike apart in the two dozen columns a tile shows', () => {
    const pieces = [
      'a',
      'b',
      'east',
      'west',
      'x'.repeat(30),
      'x'.repeat(60),
      'group-',
      'api',
      '-',
      '📁',
      '📂',
      '界'.repeat(17),
      '東',
      '西',
    ]
    let seed = 11
    const pick = (n: number) => {
      seed = (seed + 0x6d2b79f5) | 0
      let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
      return ((t ^ (t >>> 14)) >>> 0) % n
    }
    const name = () => Array.from({ length: 1 + pick(5) }, () => pieces[pick(pieces.length)]).join('')
    const wide = (c: string) => (/[\u2e80-\ua4cf\uac00-\ud7a3]|\p{Extended_Pictographic}/u.test(c) ? 2 : 1)
    const shown = (label: string) => {
      let columns = 0
      let out = ''
      for (const c of label) {
        columns += wide(c)
        if (columns > 26) break
        out += c
      }
      return out
    }
    for (let t = 0; t < 3000; t++) {
      const paths = new Set<string>()
      const n = 2 + pick(5)
      for (let tries = 0; paths.size < n && tries < 50; tries++)
        paths.add(`${pick(3) ? '/' : '~/'}${Array.from({ length: 1 + pick(3) }, name).join('/')}/api`)
      const labels = apart([...paths])
      expect(new Set(labels.map(shown)).size, [...paths].join(' ')).toBe(paths.size)
    }
  })
})

describe('snippet', () => {
  it('reads Markdown as plain words on one line', () => {
    expect(snippet('## Summary\n\n- **Fixed** the `login` [redirect](https://x.y)\n- 2. done')).toBe(
      'Summary Fixed the login redirect 2. done',
    )
    expect(snippet('Run:\n```sh\nrm -rf /\n```\nthen > quote')).toBe('Run: rm -rf / then > quote')
    expect(snippet('a'.repeat(300), 10)).toBe(`${'a'.repeat(9)}…`)
  })
})
