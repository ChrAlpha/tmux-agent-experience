import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, Failure, watch } from './api'
import type { Action, WatchDoc } from './contract'

const stream = (chunks: string[]) =>
  new ReadableStream<Uint8Array>({
    start(c) {
      for (const chunk of chunks) c.enqueue(new TextEncoder().encode(chunk))
      c.close()
    },
  })

afterEach(() => vi.unstubAllGlobals())

describe('watch', () => {
  // tmag serve's frames, as they arrive in chunks that split lines anywhere,
  // from a computer the page reaches at a path of its own.
  it('reads documents and cursors from frames in any chunks', async () => {
    const empty = ': watching\n\nid: 0\nevent: doc\ndata: {"type":"snapshot","seq":0}\n\n'
    const later =
      `${empty}event: ping\ndata: 1\n\n` +
      'id: 29\r\nevent: doc\r\ndata: {"type":"snapshot",\r\ndata: "seq":29}\r\n\r\n'
    const panes = 'event: doc\ndata: {"type":"panes","panes":[]}\n\n'
    for (const size of [1, 3, 7, later.length]) {
      const fetch = vi.fn<typeof globalThis.fetch>()
      vi.stubGlobal('fetch', fetch)
      const docs: [WatchDoc, string | undefined][] = []
      let alive = 0
      for (const text of [empty, later, panes]) {
        const chunks: string[] = []
        for (let i = 0; i < text.length; i += size) chunks.push(text.slice(i, i + size))
        fetch.mockResolvedValueOnce(new Response(stream(chunks)))
        await watch('/two', 'secret', {
          since: docs.at(-1)?.[1] ?? null,
          signal: new AbortController().signal,
          onDoc: (d, c) => docs.push([d, c]),
          onAlive: () => alive++,
        })
      }
      const options = expect.objectContaining({ headers: { Authorization: 'Bearer secret' } })
      expect(fetch.mock.calls).toEqual([
        ['/two/v2/watch', options],
        ['/two/v2/watch', options],
        ['/two/v2/watch?since=29', options],
      ])
      expect(docs, `chunks of ${size}`).toEqual([
        [{ type: 'snapshot', seq: 0 }, undefined],
        [{ type: 'snapshot', seq: 0 }, undefined],
        [{ type: 'snapshot', seq: 29 }, '29'],
        [{ type: 'panes', panes: [] }, undefined],
      ])
      expect(alive).toBe(5)
    }
  })
})

describe('act', () => {
  const pane = { server: 'default', id: '%1' }
  const terminal = { ...pane, target: 'work:1.0', cwd: '/src', command: 'claude', agent: 'claude' }
  const session = {
    id: 'a',
    agent: 'claude',
    state: 'idle',
    capabilities: ['send', 'interrupt', 'answer'],
    panes: [{ ...pane, evidence: 'process', view: 'main' }],
  }
  const send: Action = { verb: 'send', target: 'a', text: 'hello' }
  const start: Action = { verb: 'start', agent: 'claude', dir: '/src' }
  const answer: Action = { verb: 'answer', target: 'a', request: 'r', decision: 'allow' }
  const delivery = { schema: 'tmag/v2', seq: 18, session, pane: null, via: 'inbox' }
  const started = { schema: 'tmag/v2', seq: 19, session, pane, dir: '/src' }

  it.each([
    ['send', send, delivery],
    ['start', start, started],
    ['held send', send, { ...delivery, held: true }],
    [
      'pane-only keys',
      { verb: 'keys', target: '%1', screen: `1:${'0'.repeat(64)}`, keys: ['Enter'] },
      {
        ...delivery,
        via: 'pane',
        pane: terminal,
        session: { id: '', agent: '', state: '', capabilities: null, panes: null },
      },
    ],
    [
      'v0.7.0 interrupt',
      { verb: 'interrupt', target: 'a' },
      {
        ...delivery,
        via: 'pane',
        session: { ...session, state: 'working' },
        pane: { ...terminal, session: 'a', evidence: 'process', view: 'main' },
      },
    ],
    [
      'answer',
      answer,
      {
        ...delivery,
        via: 'hook',
        session: { ...session, state: 'blocked', requests: [{ id: 'r', kind: 'permission' }] },
      },
    ],
  ] satisfies [string, Action, unknown][])('returns the %s receipt unchanged', async (_, action, receipt) => {
    const fetch = vi.fn(async () => Response.json(receipt))
    vi.stubGlobal('fetch', fetch)
    await expect(act('', 't', action)).resolves.toEqual(receipt)
    expect(fetch).toHaveBeenCalledExactlyOnceWith('/v2/act', expect.objectContaining({ method: 'POST' }))
  })

  it.each([
    [200, '{"schema":"tmag/v2","seq":'],
    [200, '<html>Sign in</html>'],
    [200, '{}'],
    [200, '{"schema":"tmag/v2"}'],
    [200, '[{"schema":"tmag/v2","seq":18}]'],
    [200, '{"schema":"tmag/http/v2","seq":18}'],
    [200, '{"schema":"tmag/v2","seq":"18"}'],
    [200, 'null'],
    [204, null],
    [200, '{"caller":{"by":{"agent":"pi"}},"panes":[],"schema":"tmag/v2","seq":0,"sessions":[]}'],
    [200, '{"schema":"tmag/v2","seq":29,"type":"snapshot","sessions":[],"panes":[]}'],
    [200, JSON.stringify(started), send],
    [200, JSON.stringify(delivery), start],
    [200, JSON.stringify({ ...delivery, via: '' })],
    [200, JSON.stringify({ ...delivery, via: 7 })],
    [200, JSON.stringify({ ...started, dir: null }), start],
    [200, JSON.stringify({ ...started, session: { id: '' } }), start],
    [200, JSON.stringify({ ...started, session: { id: 7 } }), start],
    [200, JSON.stringify({ ...delivery, schema: 'tmag/http/v2' })],
    [200, JSON.stringify({ ...started, seq: '19' }), start],
  ] as const)('rejects HTTP %i with body %s', async (status, body, action: Action = answer) => {
    const fetch = vi.fn(async () => new Response(body, { status }))
    vi.stubGlobal('fetch', fetch)
    const result = act('', 't', action)
    await expect(result).rejects.toBeInstanceOf(Failure)
    await expect(result).rejects.toMatchObject({ status, code: 'receipt' })
    expect(fetch).toHaveBeenCalledOnce()
  })

  it("fails with tmag's refusal, or says that nothing answered", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>()
    vi.stubGlobal('fetch', fetch)
    fetch.mockResolvedValueOnce(
      Response.json({ schema: 'tmag/v2', error: { code: 'refused:busy', message: 'it works' } }, { status: 409 }),
    )
    await expect(act('', 't', { verb: 'interrupt', target: 'a' })).rejects.toMatchObject({
      status: 409,
      code: 'refused:busy',
    })
    expect(fetch).toHaveBeenCalledTimes(1)

    fetch.mockResolvedValueOnce(new Response('not found', { status: 404 }))
    await expect(act('', 't', { verb: 'interrupt', target: 'a' })).rejects.toThrow(
      'no tmag serve answered /v2/act (HTTP 404)',
    )
    expect(fetch).toHaveBeenCalledTimes(2)

    fetch.mockRejectedValueOnce(new TypeError('Load failed'))
    const lost = await act('', 't', { verb: 'interrupt', target: 'a' }).catch((e: unknown) => e)
    expect(lost).toBeInstanceOf(Failure)
    expect(lost).toMatchObject({ status: 0, code: 'network' })
    expect(fetch).toHaveBeenCalledTimes(3)
  })
})
