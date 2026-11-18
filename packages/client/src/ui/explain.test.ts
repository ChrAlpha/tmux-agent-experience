import { describe, expect, it } from 'vitest'
import { Failure } from '../api'
import { outcome } from './explain'

describe('outcome', () => {
  it("says a refusal in the page's words, for the verb refused", () => {
    const busy = new Failure(
      409,
      'refused:busy',
      'refused (busy): 0b00 is working; tmag does not type into a busy pane',
    )
    expect(outcome(busy, 'send')).toBe(
      'Not sent: it cannot take a prompt while it works, or has not taken the last one yet. Send it again once it is idle.',
    )
    expect(outcome(busy, 'interrupt')).toBe(
      'Not stopped: tmag sent it an interrupt moments ago, and it has not reported since.',
    )
    expect(
      outcome(new Failure(409, 'refused:changed', 'the answer hook of request r1 of 0a00 has left'), 'answer'),
    ).toBe('Not answered: it changed meanwhile. If it still asks, answer it in the terminal.')
    expect(outcome(new Failure(422, 'ended', '0a00 has ended'), 'send')).toBe('Not sent: the session has ended.')
  })

  it("keeps tmag's message for a refusal the page does not know", () => {
    expect(outcome(new Failure(409, 'refused:new', 'refused (new): why'), 'send')).toBe('refused (new): why')
    expect(outcome(new Failure(409, 'refused:busy', 'refused (busy): why'), 'answer')).toBe('refused (busy): why')
  })

  it('says that an action whose answer is lost may have happened', () => {
    const unknown =
      'The computer did not say how it went, so it may or may not have happened: check before trying again.'
    expect(outcome(new Failure(0, 'network', 'the computer did not answer'), 'send')).toBe(unknown)
    expect(outcome(new Failure(504, 'timeout', 'timed out'), 'answer')).toBe(unknown)
    expect(outcome(new Failure(502, 'backend', 'local command returned no JSON document'), 'send')).toBe(unknown)
    expect(outcome(new Failure(502, 'http', 'no tmag serve answered /v2/act (HTTP 502)'), 'interrupt')).toBe(unknown)
    expect(
      outcome(new Failure(422, 'error', 'typing into %3 failed, perhaps after the prompt was delivered'), 'send'),
    ).toBe(
      'It may or may not have happened, so check before trying again. tmag says: typing into %3 failed, perhaps after the prompt was delivered',
    )
    expect(outcome(new Failure(404, 'http', 'no tmag serve answered /v2/act (HTTP 404)'), 'send')).toBe(
      'no tmag serve answered /v2/act (HTTP 404)',
    )
  })
})
