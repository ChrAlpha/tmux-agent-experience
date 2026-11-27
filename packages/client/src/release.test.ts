import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('the release README.md installs', () => {
  it('is the version package.json has', () => {
    const { version } = JSON.parse(readFileSync('package.json', 'utf8')) as { version: string }
    const named = [...readFileSync('README.md', 'utf8').matchAll(/releases\/download\/client\/v([^\s/"]+)/g)]
    expect(named.map((m) => m[1])).toEqual([version])
  })
})
