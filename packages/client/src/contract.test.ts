import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { minimum, older } from './contract'

describe('older', () => {
  it('compares major, minor and patch as numbers', () => {
    expect(older('0.6.0', '0.7.0')).toBe(true)
    expect(older('0.7.0', '0.7.0')).toBe(false)
    expect(older('0.10.0', '0.7.0')).toBe(false)
    expect(older('v0.6.9', '0.7.0')).toBe(true)
    expect(older('1.0.0-rc.1', '0.7.0')).toBe(false)
    expect(older('dev', '0.7.0'), 'a build that names no release is not judged').toBe(false)
  })
})

describe('minimum', () => {
  it('is the tmag README.md asks for and CI tests against', () => {
    expect(readFileSync('README.md', 'utf8')).toContain(`You need tmag ${minimum} or later`)
    expect(readFileSync('../../.github/workflows/ci.yml', 'utf8')).toMatch(
      new RegExp(`CLIENT_TMAG_REF: v${minimum}\\n`),
    )
  })
})
