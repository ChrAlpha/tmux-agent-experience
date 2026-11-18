// The documents of tmag/http/v2 and the tmag/v2 documents it relays, as far as
// the page reads them. tmux-agent-experience's docs/http-api.md and its wiki's
// Architecture and Contracts page are the source; a field either may omit is
// optional here.

/**
 * The oldest tmag whose contract the page needs: `read --turns` came in
 * 0.7.0. README.md and TMAG_REF in .github/workflows/ci.yml name it too.
 */
export const minimum = '0.7.0'

// older says whether a tmag version comes before another. A version that
// does not start with major.minor.patch is no older than any.
export function older(version: string, than: string): boolean {
  const parse = (v: string) => /^v?(\d+)\.(\d+)\.(\d+)/.exec(v)?.slice(1).map(Number)
  const a = parse(version)
  const b = parse(than)
  if (!a || !b) return false
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return (a[i] ?? 0) < (b[i] ?? 0)
  return false
}

export type State = 'idle' | 'working' | 'blocked'

/** A prompt a session waits on: a permission, a question or a plan. */
export interface Request {
  id: string
  kind: string
  tool?: string
  since?: string
  /** Set when a subagent asks. */
  agent?: string
}

/** An answer a session owes the session that asked it, until it answers. */
export interface Obligation {
  id: string
  /** The session that asked. */
  to: { agent: string; session?: string }
  /** final: what the turn it started ends with; reply: a `tmag reply`. */
  kind: 'final' | 'reply'
  depth: number
  since: string
}

export interface Session {
  id: string
  agent: string
  state: State
  kind?: string
  turn?: string
  since?: string
  cwd?: string
  runtime?: string
  capabilities: string[]
  requests: Request[]
  /** The ids of its running subagents. */
  subagents: string[]
  /** The answers it owes the sessions that asked it, oldest first. */
  owes: Obligation[]
}

export interface Pane {
  server: string
  id: string
  target?: string
  cwd?: string
  command?: string
  agent?: string
  session?: string
  evidence?: string
  view?: string
}

/** A logged change; one with a state reports its session in full. */
export interface Event {
  seq: number
  time: string
  agent: string
  session: string
  event: string
  state?: State
  kind?: string
  turn?: string
  since?: string
  cwd?: string
  runtime?: string
  capabilities?: string[]
  requests?: Request[]
  subagents?: string[]
  owes?: Obligation[]
}

export type WatchDoc =
  | { type: 'snapshot'; seq: number; sessions?: Partial<Session>[]; panes?: Pane[] }
  | { type: 'event'; event: Event }
  | { type: 'panes'; panes?: Pane[] }

/** A prompt of a turn; `from` marks one a person did not type. */
export interface Prompt {
  text: string
  from?: string
}

export interface Turn {
  id: string
  prompts: Prompt[]
  reply: string
}

/** `read --turns N --json`. */
export interface TurnsDoc {
  turns: Turn[]
}

/** `read --lines N --json`: the text of the pane that shows the session. */
export interface ScreenDoc {
  text: string
}

/** `request --json`: what a request asks, with the tool's input. */
export interface RequestDoc {
  request: Request
  tool?: string
  input?: Record<string, unknown>
}

/** `GET /v2/info`: the instance tmag serve answers for. */
export interface InfoDoc {
  version: string
  /** Stable for the computer: two addresses of one computer give the same. */
  instance?: string
  /** The resources and verbs it serves: what the page may offer there. */
  operations?: string[]
}

/** The credential `POST /v2/pair` issues; the page keeps it. */
export interface Credential {
  id: string
  label: string
  token: string
}

export interface PairDoc {
  credential: Credential & { created?: string; expires?: string }
}

export type Action =
  | { verb: 'send'; target: string; text: string }
  | { verb: 'interrupt'; target: string }
  | {
      verb: 'answer'
      target: string
      request: string
      decision: 'allow' | 'deny'
      message?: string
      answers?: Record<string, string>
    }

/** The error document of tmag/http/v2 and of tmag/v2's refusals. */
export interface ErrorDoc {
  error: { code: string; message: string }
}
