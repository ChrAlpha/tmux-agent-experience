// A computer the page works with: its credential, the view its watch keeps
// current, what a session of it shows (its turns, its screen, its requests),
// read when shown, what the page sent it until its turns show it, and what
// the person has seen of it. Of all that, the page keeps only the
// credential and what was seen, in this browser's storage.

import { batch, computed, type Signal, signal } from '@preact/signals'
import * as api from './api'
import {
  type Action,
  type Credential,
  type InfoDoc,
  minimum,
  older,
  type RequestDoc,
  type ScreenDoc,
  type Turn,
  type TurnsDoc,
} from './contract'
import {
  apply,
  done,
  empty,
  find,
  firstSeen,
  follows,
  kept,
  look,
  type Seen,
  type Sent,
  sessions,
  told,
  unread,
  type View,
} from './view'

/** A computer as the page keeps it. */
export interface Saved {
  /** '' for the computer that serves the page; else where it is proxied. */
  name: string
  credential: Credential
  /** Its instance, once the page read it. */
  instance?: string
}

/** What a read gave, or why it failed; a failed read keeps what came before. */
export interface Read<T> {
  value?: T
  error?: Error
}

/** What the store does as a computer learns of itself. */
export interface Hooks {
  /** What the page keeps of it changed. */
  saved(c: Computer): void
  /** It no longer accepts this device. */
  lost(c: Computer, why: string): void
  /** It said which instance it is, which no other computer may be. */
  identified(c: Computer): void
}

export class Computer {
  readonly name: string
  /** Where its API is on the page's origin. */
  readonly base: string
  readonly credential: Credential
  instance: string | undefined

  /** What the watch rebuilt. */
  readonly view = signal<View>(empty)
  /** Whether a snapshot arrived: until then, no session is known to be gone. */
  readonly loaded = signal(false)
  /** Whether the watch delivers. */
  readonly live = signal(false)
  /** Why it does not. */
  readonly problem = signal<Error | null>(null)
  /** What it said of itself when the watch last started. */
  readonly info = signal<InfoDoc | null>(null)
  /** What the person has seen of it, once its instance is known. */
  readonly seen = signal<Seen | null>(null)
  /** Requests answered from this page, hidden before the watch says so. */
  readonly answered = signal<ReadonlySet<string>>(new Set())

  /** Its version, when it is older than the page needs. */
  readonly outdated = computed(() => {
    const v = this.info.value?.version
    return v && older(v, minimum) ? v : ''
  })
  /** Its sessions that ended a turn the person has not seen end. */
  readonly done = computed(() => {
    const seen = this.seen.value
    return seen ? sessions(this.view.value).filter((s) => done(s, seen)).length : 0
  })

  readonly #hooks: Hooks
  /** What storage held of what was seen: not looked up yet, none, or that. */
  #stored: Seen | null | undefined = undefined
  #cursor: string | null = null
  #stream: AbortController | null = null
  #retry: ReturnType<typeof setTimeout> | undefined
  #ask: ReturnType<typeof setTimeout> | undefined
  /** Whether it said which instance it is since the page made it. */
  #identified = false
  #failures = 0
  #heard = 0
  #gone = false
  readonly #turns = new Map<string, Signal<Read<Turn[]>>>()
  readonly #screens = new Map<string, Signal<Read<ScreenDoc>>>()
  readonly #requests = new Map<string, Signal<Read<RequestDoc>>>()
  readonly #sent = new Map<string, Signal<readonly Sent[]>>()
  #sends = 0
  readonly #running = new Map<Signal<unknown>, { again: boolean }>()

  constructor(saved: Saved, hooks: Hooks) {
    this.name = saved.name
    this.base = saved.name ? `/${saved.name}` : ''
    this.credential = saved.credential
    this.instance = saved.instance
    this.#hooks = hooks
    if (saved.instance) this.#lookUp(saved.instance)
  }

  /** How the page keeps it. */
  saved(): Saved {
    return { name: this.name, credential: this.credential, ...(this.instance ? { instance: this.instance } : {}) }
  }

  // can says whether it serves an operation: where it does not say, only
  // what every computer the page works with serves.
  can(operation: string): boolean {
    const ops = this.info.value?.operations
    return ops
      ? ops.includes(operation)
      : ['list', 'inspect', 'watch', 'read', 'request', 'send', 'interrupt', 'answer'].includes(operation)
  }

  // authorized runs a request with the credential. A computer that no longer
  // accepts it is forgotten.
  async #authorized<T>(run: (token: string) => Promise<T>): Promise<T> {
    try {
      return await run(this.credential.token)
    } catch (e) {
      if (e instanceof api.Failure && e.status === 401 && !this.#gone) {
        this.#hooks.lost(this, 'no longer accepts this device: its credential was revoked or expired. Pair it again.')
      }
      throw e
    }
  }

  // The watch: one stream while the page works with the computer, resumed
  // from its cursor after it breaks, sooner the less it has failed. tmag
  // pings every 15 s, so a stream quiet for 45 s is gone even if the network
  // did not say so. A computer shows nothing until it said which instance it
  // is, so that the page never works with one computer twice.
  async connect(): Promise<void> {
    clearTimeout(this.#retry)
    if (this.#stream || this.#gone) return
    // One attempt owns the connection from here, its identification too.
    const ctl = new AbortController()
    this.#stream = ctl
    this.#heard = Date.now()
    if (!this.#identified) {
      try {
        const info = await this.#authorized((t) => api.get<InfoDoc>(this.base, t, 'info', {}, ctl.signal))
        if (!this.#gone) this.learn(info)
      } catch (e) {
        if (!ctl.signal.aborted) this.problem.value = e as Error
      }
      if (this.#stream !== ctl || this.#gone) return
      if (!this.#identified) {
        this.#stream = null
        if (ctl.signal.aborted) return this.connect() // the page woke: at once
        this.#failures++
        this.#retry = setTimeout(() => this.connect(), Math.min(15_000, 500 * 2 ** this.#failures))
        return
      }
    }
    const quiet = setInterval(() => Date.now() - this.#heard > 45_000 && ctl.abort(), 5_000)
    try {
      await this.#authorized((token) =>
        api.watch(this.base, token, {
          since: this.#cursor,
          signal: ctl.signal,
          onAlive: () => {
            this.#heard = Date.now()
            this.#failures = 0
            if (!this.live.value) {
              batch(() => {
                this.live.value = true
                this.problem.value = null
              })
              void this.#check()
            }
          },
          onDoc: (doc, at) => {
            if (at) this.#cursor = at
            batch(() => {
              if (doc.type === 'snapshot') this.loaded.value = true
              this.view.value = apply(this.view.value, doc)
              if (doc.type !== 'snapshot') return
              if (this.seen.value) this.#see(kept(this.seen.value, this.view.value))
              else this.#seenReady()
            })
          },
        }),
      )
    } catch (e) {
      this.problem.value = e as Error
    } finally {
      clearInterval(quiet)
      if (this.#stream === ctl) this.#stream = null
      this.live.value = false
    }
    if (this.#gone || this.#stream) return
    if (ctl.signal.aborted) return this.connect() // fell quiet, or the page woke: at once
    this.#failures++
    this.#retry = setTimeout(() => this.connect(), Math.min(15_000, 500 * 2 ** this.#failures))
  }

  // check reads what the computer says of itself each time the watch starts:
  // it may have updated tmag, or gone back to an older one, meanwhile.
  async #check() {
    try {
      const info = await this.#authorized((t) => api.get<InfoDoc>(this.base, t, 'info', {}))
      if (this.#gone) return
      this.learn(info)
    } catch {
      // The watch says why the computer does not answer. One that delivers
      // and says nothing of itself is asked again: which instance it is
      // decides whether the page works with it twice.
      clearTimeout(this.#ask)
      if (!this.#gone && this.live.value) this.#ask = setTimeout(() => void this.#check(), 10_000)
    }
  }

  /** What the computer said of itself, which names the instance it is. */
  learn(info: InfoDoc) {
    if (this.#gone) return
    this.info.value = info
    if (info.instance) {
      if (info.instance !== this.instance) {
        this.instance = info.instance
        this.seen.value = null
        this.#lookUp(info.instance)
        this.#hooks.saved(this)
      }
      this.#hooks.identified(this)
    }
    this.#identified = !this.#gone
  }

  // What the person has seen of the computer is kept under its instance, so
  // that it survives the computer moving to another address. Until storage
  // was read, nothing is seen; then what it held, of the sessions the watch
  // shows; or, with nothing stored, what the first snapshot shows ended.
  #lookUp(instance: string) {
    let stored: unknown = null
    try {
      stored = JSON.parse(localStorage.getItem(`tmag.seen.${instance}`) ?? 'null')
    } catch {}
    const valid =
      !!stored &&
      typeof stored === 'object' &&
      !Array.isArray(stored) &&
      Object.values(stored).every((t) => typeof t === 'string')
    this.#stored = valid ? (stored as Seen) : null
    this.#seenReady()
  }

  #seenReady() {
    if (this.seen.value || this.#stored === undefined) return
    if (this.#stored) this.#see(this.loaded.value ? kept(this.#stored, this.view.value) : this.#stored)
    else if (this.loaded.value) this.#see(firstSeen(this.view.value))
  }

  #see(seen: Seen) {
    if (seen === this.seen.value) return
    this.seen.value = seen
    if (this.instance) localStorage.setItem(`tmag.seen.${this.instance}`, JSON.stringify(seen))
  }

  /** The person looks at a session: the turn it ended is seen, once what was seen is known. */
  looked(id: string) {
    const s = find(this.view.value, id)
    const seen = this.seen.value
    if (s && seen) this.#see(look(seen, s))
  }

  stop() {
    clearTimeout(this.#retry)
    clearTimeout(this.#ask)
    this.#stream?.abort()
    this.#stream = null
    this.live.value = false
  }

  /** The page forgets the computer: it stops, and reads nothing more. */
  forget() {
    this.#gone = true
    this.stop()
  }

  // wake resumes the watch at once when the page shows again: a phone may
  // have suspended it in the background without ending it.
  wake() {
    if (this.#stream && Date.now() - this.#heard > 20_000) this.#stream.abort()
    else void this.connect()
  }

  #slot<T>(m: Map<string, Signal<Read<T>>>, key: string): Signal<Read<T>> {
    const s = m.get(key) ?? signal<Read<T>>({})
    m.set(key, s)
    return s
  }

  turnsOf = (id: string) => this.#slot(this.#turns, id)
  screenOf = (id: string) => this.#slot(this.#screens, id)
  requestOf = (id: string) => this.#slot(this.#requests, id)

  /** What the page sent a session that its turns do not show yet. */
  sentTo(id: string): Signal<readonly Sent[]> {
    const s = this.#sent.get(id) ?? signal<readonly Sent[]>([])
    this.#sent.set(id, s)
    return s
  }

  // read runs load now, or once more after the run in flight for the same
  // slot, so that what it shows is never older than what asked for it. What
  // it read goes to got as it shows. A forgotten computer reads nothing.
  #read<T>(into: Signal<Read<T>>, load: (token: string) => Promise<T>, got?: (value: T) => void) {
    if (this.#gone) return
    const run = this.#running.get(into)
    if (run) {
      run.again = true
      return
    }
    const next = { again: false }
    this.#running.set(into, next)
    void (async () => {
      do {
        next.again = false
        if (this.#gone) break
        try {
          const value = await this.#authorized(load)
          if (this.#gone) break
          batch(() => {
            into.value = { value }
            got?.(value)
          })
        } catch (e) {
          if (this.#gone) break
          into.value = { ...into.value, error: e as Error }
        }
      } while (next.again)
      this.#running.delete(into)
    })()
  }

  loadTurns(id: string) {
    this.#read(
      this.turnsOf(id),
      async (t) => (await api.get<TurnsDoc>(this.base, t, 'read', { target: id, turns: 20 })).turns,
      (turns) => this.#settle(id, turns),
    )
  }

  // settle keeps what was sent to a session that its turns do not show yet.
  #settle(id: string, turns: readonly Turn[]) {
    const into = this.#sent.get(id)
    if (into?.value.length) into.value = unread(into.value, turns, find(this.view.value, id))
  }

  loadScreen(target: string) {
    this.#read(this.screenOf(target), (t) => api.get<ScreenDoc>(this.base, t, 'read', { target, lines: 120 }))
  }

  loadRequest(session: string, request: string) {
    this.#read(this.requestOf(request), (t) =>
      api.get<RequestDoc>(this.base, t, 'request', { target: session, request }),
    )
  }

  async act<T = unknown>(action: Action): Promise<T> {
    // A prompt is known by its text and the session's state as it goes,
    // before the session can start on it.
    const send = action.verb === 'send'
    const shown = send ? (this.turnsOf(action.target).value.value ?? []) : []
    const text = send ? action.text.trim() : ''
    const was = 'target' in action ? find(this.view.value, action.target) : undefined
    const result = await this.#authorized((t) => api.act<T>(this.base, t, action))
    if (action.verb === 'answer') this.answered.value = new Set(this.answered.value).add(action.request)
    if (send && follows(text)) {
      const into = this.sentTo(action.target)
      into.value = [...into.value, { id: ++this.#sends, text, seen: told(shown, text), since: was?.since }]
      this.#settle(action.target, this.turnsOf(action.target).value.value ?? [])
    }
    return result
  }
}
