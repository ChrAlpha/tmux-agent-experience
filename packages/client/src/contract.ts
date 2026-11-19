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

/**
 * `read --lines N --json`: what the pane shows, as text and, where tmag gives
 * them, with its terminal's colours and the digest keys are typed against.
 */
export interface ScreenDoc {
  text: string
  /** The same lines with their SGR escapes. */
  ansi?: string
  /** What `keys --screen` takes: this screen, as it was read. */
  screen?: string
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

/** A file the work tree of a session's directory changed against HEAD. */
export interface Changed {
  path: string
  /** git's two status letters, staged then unstaged; "??" untracked. */
  status: string
  added?: number
  removed?: number
  binary?: boolean
  /** Kept through a filter tmag does not run: no counts, no patch. */
  filtered?: boolean
}

/** `changes --json`: what a session's directory changed. */
export interface ChangesDoc {
  changes: { dir: string; top: string; files: Changed[]; truncated?: boolean }
}

/** `changes --file PATH --json`: one file's patch. */
export interface PatchDoc {
  file: string
  patch: string
}

/**
 * The git repository a directory is in: its work tree's top, the main
 * worktree, and the branch its work tree has checked out, none when detached.
 */
export interface Repo {
  top: string
  main?: string
  branch?: string
}

/** A directory sessions ran in, when one last did, and its repository. */
export interface Place {
  dir: string
  at: string
  repo?: Repo
}

/** A template the person set as `@tmag-start-NAME`, and the agent it runs. */
export interface Template {
  name: string
  agent: string
}

/** `dirs --json`: where an agent may start. */
export interface PlacesDoc {
  places: { home?: string; recent: Place[]; templates: Template[] }
}

/** `dirs --json DIR`: a directory's subdirectories and its repository. */
export interface DirDoc {
  dir: {
    path: string
    parent?: string
    repo?: Repo
    dirs: { name: string; repo?: boolean }[]
    truncated?: boolean
  }
}

/** A pane: on which tmux server, and which. */
export interface PaneRef {
  server: string
  id: string
}

/** `start --json`: the pane a started agent runs in, and its session. */
export interface StartDoc {
  pane: PaneRef
  session: Pick<Session, 'id' | 'agent'>
  dir: string
}

/** The agents `start` runs by name. */
export const startable = ['claude', 'codex', 'pi', 'omp'] as const

/** The keys `keys` presses, by tmux's names. */
export const keyNames = [
  'Enter',
  'Escape',
  'Tab',
  'BTab',
  'Up',
  'Down',
  'Left',
  'Right',
  'Space',
  'BSpace',
  'C-c',
  'y',
  'n',
  '0',
  '1',
  '2',
  '3',
  '4',
  '5',
  '6',
  '7',
  '8',
  '9',
] as const
export type Key = (typeof keyNames)[number]

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
  | {
      verb: 'start'
      agent?: (typeof startable)[number]
      template?: string
      dir: string
      worktree?: string
      text?: string
    }
  | { verb: 'keys'; target: string; screen: string; keys: Key[]; text?: string }

/** The error document of tmag/http/v2 and of tmag/v2's refusals. */
export interface ErrorDoc {
  error: { code: string; message: string }
  /** A start whose agent started and whose prompt then failed: its session. */
  started?: Pick<Session, 'id' | 'agent'>
  /** A start whose agent showed no session in time: the pane it waits in. */
  pane?: PaneRef
}
