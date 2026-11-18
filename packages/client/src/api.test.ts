import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, Failure, watch } from './api'
import type { WatchDoc } from './contract'

const stream = (chunks: string[]) =>
  new ReadableStream<Uint8Array>({
    start(c) {
      for (const chunk of chunks) c.enqueue(new TextEncoder().encode(chunk))
      c.close()
    },
  })

afterEach(() => vi.unstubAllGlobals())

describe('watch', () => {
  // tmag serve's frames, as they arrive in chunks that split lines anywhere.
  it('reads documents and cursors from frames in any chunks', async () => {
    const text =
      ': watching\n\nid: 18\nevent: doc\ndata: {"type":"panes","panes":[]}\n\nevent: ping\ndata: 1\n\n' +
      'id: 29\r\nevent: doc\r\ndata: {"type":"snapshot",\r\ndata: "seq":29}\r\n\r\n'
    for (const size of [1, 3, 7, text.length]) {
      const chunks: string[] = []
      for (let i = 0; i < text.length; i += size) chunks.push(text.slice(i, i + size))
      const fetch = vi.fn(async () => new Response(stream(chunks)))
      vi.stubGlobal('fetch', fetch)
      const docs: [WatchDoc, string | undefined][] = []
      let alive = 0
      await watch('secret', {
        since: '17',
        signal: new AbortController().signal,
        onDoc: (d, c) => docs.push([d, c]),
        onAlive: () => alive++,
      })
      expect(docs, `chunks of ${size}`).toEqual([
        [{ type: 'panes', panes: [] }, '18'],
        [{ type: 'snapshot', seq: 29 }, '29'],
      ])
      expect(alive).toBe(3)
      expect(fetch).toHaveBeenCalledWith(
        '/v2/watch?since=17',
        expect.objectContaining({ headers: { Authorization: 'Bearer secret' } }),
      )
    }
  })
})

describe('act', () => {
  it("fails with tmag's refusal, or says that nothing answered", async () => {
    vi.stubGlobal('fetch', async () =>
      Response.json({ schema: 'tmag/v2', error: { code: 'refused:busy', message: 'it works' } }, { status: 409 }),
    )
    await expect(act('t', { verb: 'interrupt', target: 'a' })).rejects.toMatchObject({
      status: 409,
      code: 'refused:busy',
    })

    vi.stubGlobal('fetch', async () => new Response('not found', { status: 404 }))
    await expect(act('t', { verb: 'interrupt', target: 'a' })).rejects.toThrow(
      'no tmag serve answered /v2/act (HTTP 404)',
    )

    vi.stubGlobal('fetch', async () => {
      throw new TypeError('Load failed')
    })
    const lost = await act('t', { verb: 'interrupt', target: 'a' }).catch((e: unknown) => e)
    expect(lost).toBeInstanceOf(Failure)
    expect(lost).toMatchObject({ status: 0, code: 'network' })
  })
})
