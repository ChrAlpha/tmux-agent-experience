// The tmag/http/v2 contract, which tmag serve answers under /v2/ on the page's
// own origin. Every request but pairing proves the credential pairing issued,
// in the Authorization header and never in a URL.

import { EventSourceParserStream } from 'eventsource-parser/stream'
import type { Action, Credential, ErrorDoc, PairDoc, WatchDoc } from './contract'

// Failure is a refused or failed request: the HTTP status (0 when nothing
// answered), and the code and message of the error document when there is one.
export class Failure extends Error {
  readonly status: number
  readonly code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

const isError = (doc: unknown): doc is ErrorDoc =>
  typeof doc === 'object' && doc !== null && typeof (doc as ErrorDoc).error?.code === 'string'

async function failure(res: Response, path: string): Promise<Failure> {
  const doc: unknown = await res.json().catch(() => null)
  if (isError(doc)) return new Failure(res.status, doc.error.code, doc.error.message)
  return new Failure(res.status, 'http', `no tmag serve answered ${path} (HTTP ${res.status})`)
}

// request fetches a path and fails with what tmag serve said, or with what
// kept it from answering. An aborted request fails with the abort.
async function request(path: string, init: RequestInit): Promise<Response> {
  let res: Response
  try {
    res = await fetch(path, { cache: 'no-store', ...init })
  } catch (err) {
    if (init.signal?.aborted) throw err
    throw new Failure(0, 'network', `the computer did not answer: ${(err as Error).message}`)
  }
  if (!res.ok) throw await failure(res, path.split('?')[0] ?? path)
  return res
}

const bearer = (token: string) => ({ Authorization: `Bearer ${token}` })
const json = { 'Content-Type': 'application/json' }

// get reads a resource (info, list, inspect, request or read) with its
// parameters.
export async function get<T>(token: string, resource: string, params: Record<string, string | number>): Promise<T> {
  const query = new URLSearchParams(Object.entries(params).map(([k, v]) => [k, String(v)]))
  const res = await request(`/v2/${resource}${query.size ? `?${query}` : ''}`, { headers: bearer(token) })
  return res.json()
}

// act runs send, interrupt or answer. A lost answer is an unknown outcome:
// the caller reconciles with the watch and never tries again by itself.
export async function act(token: string, action: Action): Promise<void> {
  await request('/v2/act', { method: 'POST', headers: { ...bearer(token), ...json }, body: JSON.stringify(action) })
}

// pair redeems a code from `tmag auth pair` for a credential of its own.
export async function pair(code: string): Promise<Credential> {
  const res = await request('/v2/pair', { method: 'POST', headers: json, body: JSON.stringify({ code }) })
  const { id, label, token } = ((await res.json()) as PairDoc).credential
  return { id, label, token }
}

export interface Watcher {
  /** The cursor to resume after, if any. */
  since: string | null
  signal: AbortSignal
  /** Each watch document, with the cursor it advances to, if it does. */
  onDoc(doc: WatchDoc, cursor: string | undefined): void
  /** Each frame, keepalives included: the stream is alive. */
  onAlive(): void
}

// watch follows /v2/watch until the stream ends or w.signal aborts it.
export async function watch(token: string, w: Watcher): Promise<void> {
  const query = w.since ? `?since=${encodeURIComponent(w.since)}` : ''
  let res: Response
  try {
    res = await request(`/v2/watch${query}`, { headers: bearer(token), signal: w.signal })
  } catch (err) {
    if (w.signal.aborted) return
    throw err
  }
  if (!res.body) throw new Failure(0, 'network', 'the stream had no body')
  const frames = res.body
    .pipeThrough(new TextDecoderStream())
    .pipeThrough(new EventSourceParserStream({ onError: 'terminate' }))
    .getReader()
  try {
    for (;;) {
      const { value, done } = await frames.read()
      if (done) return
      w.onAlive()
      if (value.event === 'doc') w.onDoc(JSON.parse(value.data) as WatchDoc, value.id || undefined)
    }
  } catch (err) {
    if (!w.signal.aborted) throw new Failure(0, 'network', `the stream broke: ${(err as Error).message}`)
  }
}
