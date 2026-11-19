// A computer the page works with: its credential, the view its watch keeps
// current, what a session of it shows (its turns, its screen, its requests,
// its directory's changes) and where its agents may start (the places its
// sessions ran in, with their repositories, and its directories), read when
// shown, what the page sent it until its turns show it, and what the person
// has seen of it. Of all that, the page keeps only the
// credential and what was seen, in this browser's storage.

import { batch, computed, effect, type Signal, signal } from '@preact/signals'
import * as api from './api'
import {
  type Action,
  type ChangesDoc,
  type Credential,
  type DirDoc,
  type InfoDoc,
  minimum,
  older,
  type PatchDoc,
  type PlacesDoc,
  type Repo,
  type RequestDoc,
  type ScreenDoc,
  type Turn,
  type TurnsDoc,
} from './contract'
import { type Comment, isComment } from './patch'
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

/**
 * The sessions a prompt or an interrupt is on its way to, by the instance
 * they are of, whichever screen and whichever address of it sent it.
 */
const sending = signal<ReadonlySet<string>>(new Set())

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
  /** When the read that gave value began, as Computer.begun counts. */
  began?: number
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
  /** Where its agents may start, and the repositories of its sessions' directories. */
  readonly places = signal<Read<PlacesDoc['places']>>({})

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
  /** How many times the page asked the computer what it is. */
  #asked = 0
  #heard = 0
  #gone = false
  readonly #turns = new Map<string, Signal<Read<Turn[]>>>()
  readonly #screens = new Map<string, Signal<Read<ScreenDoc>>>()
  readonly #requests = new Map<string, Signal<Read<RequestDoc>>>()
  readonly #changes = new Map<string, Signal<Read<ChangesDoc['changes']>>>()
  readonly #patches = new Map<string, Signal<Read<string>>>()
  readonly #last = new Map<string, Signal<Read<Turn | null>>>()
  readonly #lastAt = new Map<string, string>()
  readonly #dirs = new Map<string, Signal<Read<DirDoc['dir']>>>()
  /** The directories of sessions a read of the places began after it saw. */
  readonly #placed = new Set<string>()
  readonly #sent = new Map<string, Signal<readonly Sent[]>>()
  #sends = 0
  readonly #drafts = new Map<string, Signal<string>>()
  readonly #comments = new Map<string, Signal<readonly Comment[]>>()
  readonly #writing = new Map<string, Signal<Comment | null>>()
  readonly #running = new Map<Signal<unknown>, { again: boolean }>()
  #begun = 0

  constructor(saved: Saved, hooks: Hooks) {
    this.name = saved.name
    this.base = saved.name ? `/${saved.name}` : ''
    this.credential = saved.credential
    this.instance = saved.instance
    this.#hooks = hooks
    if (saved.instance) {
      this.#keptFrom()
      this.#lookUp(saved.instance)
    }
  }

  /** How the page keeps it. */
  saved(): Saved {
    return { name: this.name, credential: this.credential, ...(this.instance ? { instance: this.instance } : {}) }
  }

  // can says whether it serves an operation: where it does not say, only
  // what every computer the page works with serves.
  can(operation: string): boolean {
    // One that said what it is but not which computer it is serves nothing.
    if (this.info.value && !this.info.value.instance) return false
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
        const asked = ++this.#asked
        const info = await this.#authorized((t) => api.get<InfoDoc>(this.base, t, 'info', {}, ctl.signal))
        if (!this.#gone && asked === this.#asked) this.learn(info)
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
              if (doc.type !== 'panes') this.#placesFor(doc.type === 'snapshot')
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
      const asked = ++this.#asked
      const info = await this.#authorized((t) => api.get<InfoDoc>(this.base, t, 'info', {}))
      // Only what it said last counts: an older answer may be another instance's.
      if (this.#gone || asked !== this.#asked) return
      this.learn(info)
    } catch {
      // The watch says why the computer does not answer. One that delivers
      // and says nothing of itself is asked again: which instance it is
      // decides whether the page works with it twice.
      clearTimeout(this.#ask)
      if (!this.#gone && this.live.value) this.#ask = setTimeout(() => void this.#check(), 10_000)
    }
  }

  /** What the computer says it is, if no later question of the page's was asked meanwhile. */
  async ask(signal?: AbortSignal): Promise<InfoDoc | null> {
    const asked = ++this.#asked
    const info = await api.get<InfoDoc>(this.base, this.credential.token, 'info', {}, signal).catch(() => null)
    return asked === this.#asked && !this.#gone ? info : null
  }

  /** What the computer said of itself, which names the instance it is. */
  learn(info: InfoDoc) {
    if (this.#gone) return
    this.info.value = info
    // A computer is the instance its info names: one that names none is no
    // computer the page works with, and shows nothing until it names one.
    if (!info.instance) {
      this.#identified = false
      batch(() => {
        this.view.value = empty
        this.loaded.value = false
        // What it shows again comes from a snapshot of its own.
        this.#cursor = null
        this.problem.value = new Error(
          'tmag on the computer does not say which computer it is, which the page needs. Update it.',
        )
      })
      // A watch that delivers ends, and the computer is asked again.
      if (this.live.value) this.#stream?.abort()
      return
    }
    if (info.instance !== this.instance) {
      // What the person wrote is kept by the instance it was for: the
      // instance here reads what was written for it.
      this.#drafts.clear()
      this.#comments.clear()
      this.#writing.clear()
      this.#sent.clear()
      this.instance = info.instance
      this.#keptFrom()
      this.seen.value = null
      this.#lookUp(info.instance)
      this.#hooks.saved(this)
    }
    this.#hooks.identified(this)
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
  changesOf = (id: string) => this.#slot(this.#changes, id)
  patchOf = (id: string, file: string) => this.#slot(this.#patches, `${id}\u0000${file}`)
  /** A session's last turn, read for the lists. */
  lastOf = (id: string) => this.#slot(this.#last, id)
  /** A directory's subdirectories and repository. */
  dirOf = (path: string) => this.#slot(this.#dirs, path)

  /**
   * A session's last turn as last read, by its own read or by the read of
   * its turns, whichever began later: null for none, undefined before either.
   */
  latest(id: string): Turn | null | undefined {
    const one = this.lastOf(id).value
    const all = this.turnsOf(id).value
    if (all.value && (all.began ?? 0) > (one.began ?? 0)) return all.value[all.value.length - 1] ?? null
    return one.value
  }

  /** The repository a directory is in, as tmag last said, if it did. */
  repoAt(dir: string): Repo | undefined {
    const listed = this.dirOf(dir).value.value
    if (listed) return listed.repo
    return this.places.value.value?.recent.find((p) => p.dir === dir)?.repo
  }

  // scope is what the person's writing is kept under: the instance it is for,
  // whichever address it is at, as what they saw is; an address only until
  // the computer said which instance it is.
  #scope(): string {
    return this.instance ? `i:${this.instance}` : `a:${this.name}`
  }

  // keptFrom takes on what a page that kept the person's writing by address
  // kept at this one, as the instance it now names.
  #keptFrom() {
    for (const kind of ['draft', 'comments', 'writing']) {
      const prefix = `tmag.${kind}.${this.name}/`
      for (const key of Object.keys(sessionStorage)) {
        if (!key.startsWith(prefix)) continue
        const to = `tmag.${kind}.${this.#scope()}/${key.slice(prefix.length)}`
        const was = sessionStorage.getItem(key)
        if (was && !sessionStorage.getItem(to)) sessionStorage.setItem(to, was)
        sessionStorage.removeItem(key)
      }
    }
  }

  /** Keeps a prompt for a session of an instance the page does not show now, should it show it again. */
  static keepFor(instance: string, id: string, text: string) {
    const key = `tmag.draft.i:${instance}/${id}`
    const was = sessionStorage.getItem(key)
    sessionStorage.setItem(key, was ? `${text}\n\n${was}` : text)
  }

  // kept is a signal of what the person writes for a session, kept in this
  // tab's storage under key, as save writes it, while it holds something.
  #kept<T>(m: Map<string, Signal<T>>, id: string, key: string, load: (kept: string) => T, save: (v: T) => string) {
    const s = m.get(id)
    if (s) return s
    const kept = sessionStorage.getItem(key)
    let start = load('')
    try {
      if (kept) start = load(kept)
    } catch {}
    const held = signal<T>(start)
    effect(() => {
      const v = save(held.value)
      if (v) sessionStorage.setItem(key, v)
      else sessionStorage.removeItem(key)
    })
    m.set(id, held)
    return held
  }

  /** The prompt the person is writing to a session, or one a pane holds for the session it will show. */
  draftOf = (id: string) =>
    this.#kept(
      this.#drafts,
      id,
      `tmag.draft.${this.#scope()}/${id}`,
      (kept) => kept,
      (v) => v,
    )

  /** What the person says of the lines of a session's changes, until it goes in a prompt. */
  commentsOf = (id: string) =>
    this.#kept<readonly Comment[]>(
      this.#comments,
      id,
      `tmag.comments.${this.#scope()}/${id}`,
      (kept) => {
        const said: unknown = kept ? JSON.parse(kept) : []
        return Array.isArray(said) ? said.filter(isComment) : []
      },
      (v) => (v.length ? JSON.stringify(v) : ''),
    )

  /** The comment the person is writing on a line of a session's changes, if one. */
  writingOf = (id: string) =>
    this.#kept<Comment | null>(
      this.#writing,
      id,
      `tmag.writing.${this.#scope()}/${id}`,
      (kept) => {
        const writing: unknown = kept ? JSON.parse(kept) : null
        return isComment(writing) ? writing : null
      },
      (v) => (v ? JSON.stringify(v) : ''),
    )

  #going = (id: string) => `${this.#scope()}\u0000${id}`

  /** Whether a prompt or an interrupt is on its way to a session of the instance it is now. */
  isSending = (id: string) => sending.value.has(this.#going(id))

  /** What the page sent a session that its turns do not show yet. */
  sentTo(id: string): Signal<readonly Sent[]> {
    const s = this.#sent.get(id) ?? signal<readonly Sent[]>([])
    this.#sent.set(id, s)
    return s
  }

  /** How many reads began: a read whose began is above a count taken now began after it. */
  get begun() {
    return this.#begun
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
        const began = ++this.#begun
        try {
          const value = await this.#authorized(load)
          if (this.#gone) break
          batch(() => {
            into.value = { value, began }
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

  loadChanges(id: string) {
    this.#read(
      this.changesOf(id),
      async (t) => (await api.get<ChangesDoc>(this.base, t, 'changes', { target: id })).changes,
    )
  }

  loadPatch(id: string, file: string) {
    this.#read(
      this.patchOf(id, file),
      async (t) => (await api.get<PatchDoc>(this.base, t, 'changes', { target: id, file })).patch,
    )
  }

  // loadLast reads a session's last turn for the lists, once for each state
  // and turn of the session it is read for.
  loadLast(id: string, at: string) {
    if (this.#lastAt.get(id) === at && !this.lastOf(id).value.error) return
    this.#lastAt.set(id, at)
    this.#read(
      this.lastOf(id),
      async (t) => (await api.get<TurnsDoc>(this.base, t, 'read', { target: id, turns: 1 })).turns.at(-1) ?? null,
    )
  }

  loadPlaces() {
    this.#read(this.places, async (t) => (await api.get<PlacesDoc>(this.base, t, 'dirs', {})).places)
  }

  loadDir(path: string) {
    this.#read(this.dirOf(path), async (t) => (await api.get<DirDoc>(this.base, t, 'dirs', { dir: path })).dir)
  }

  // placesFor reads where agents start with each snapshot, and once the
  // watch shows a session in a directory no read of them asked about yet:
  // the repository it is in decides its project.
  #placesFor(snapshot: boolean) {
    if (!this.can('dirs')) return
    const fresh = [...this.view.value.sessions.values()].flatMap((s) =>
      s.cwd && !this.#placed.has(s.cwd) ? [s.cwd] : [],
    )
    if (!snapshot && !fresh.length) return
    for (const dir of fresh) this.#placed.add(dir)
    this.loadPlaces()
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
    const instance = this.instance
    const on = action.verb === 'send' || action.verb === 'interrupt' ? this.#going(action.target) : undefined
    if (on) sending.value = new Set(sending.value).add(on)
    let result: T
    try {
      result = await this.#authorized((t) => api.act<T>(this.base, t, action))
    } finally {
      if (on) {
        const left = new Set(sending.value)
        left.delete(on)
        sending.value = left
      }
    }
    // What another instance answers at this address since is not this one's.
    if (this.instance !== instance) return result
    if (action.verb === 'answer') this.answered.value = new Set(this.answered.value).add(action.request)
    if (send && follows(text)) {
      const into = this.sentTo(action.target)
      into.value = [...into.value, { id: ++this.#sends, text, seen: told(shown, text), since: was?.since }]
      this.#settle(action.target, this.turnsOf(action.target).value.value ?? [])
    }
    return result
  }
}
