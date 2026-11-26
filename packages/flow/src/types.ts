// What a plan sees. A plan is a module whose default export takes a Flow:
//
//   import type { Flow } from 'tmag-flow'
//   export default async function plan(flow: Flow) { ... }
//
// It imports nothing else of tmag-flow: everything is on the flow, so a
// runner installed anywhere runs it.

export interface Flow {
  // The run's name, its directory, and the project it runs in: the main
  // worktree of the repository it was started in.
  readonly run: string
  readonly dir: string
  readonly project: string
  // The --arg values the run was started with.
  readonly args: Readonly<Record<string, string>>

  // role declares a role an agent plays. Nothing starts until the role is
  // first asked or bound; when its session ends, the next ask starts another.
  role(name: string, spec: RoleSpec): Role
  // bind starts the role's agent if it has no session, and says where it runs.
  bind(role: Role): Promise<Binding>
  // ask gives the role's agent a turn with prompt and returns the reply that
  // ended that turn.
  ask(key: string, role: Role, prompt: string, options?: AskOptions): Promise<Answer>
  // gate runs a check, a command without effects: a run cut short runs it again.
  gate(key: string, command: string, options?: CommandOptions): Promise<CommandResult>
  // exec runs a command with effects: one cut short halts for a decision.
  exec(key: string, command: string, options?: CommandOptions): Promise<CommandResult>
  // decide halts the run until someone picks one of choices with
  // tmag-flow decide, and returns the choice.
  decide(key: string, question: string, choices: readonly string[]): Promise<string>
  // freeze reads a file once: every replay returns what it held then.
  freeze(key: string, path: string): Promise<string>
  // path names a file in the run's artifacts directory, its parent created.
  path(...segments: string[]): string
  // log prints a line of progress.
  log(message: string): void
}

export interface RoleSpec {
  // An agent (claude, codex, pi, omp) or a template the person set in tmux
  // as @tmag-start-NAME.
  agent: string
  // Where it runs: a directory (relative to the project), by default the
  // project; or a git worktree of its own on this branch.
  dir?: string
  worktree?: string
}

export interface Role {
  readonly name: string
  readonly spec: Readonly<RoleSpec>
}

export interface Binding {
  readonly session: string
  readonly agent: string
  readonly dir: string
  readonly window?: string
}

export interface AskOptions {
  // How long the turn may take before the run halts (default 1h, or the
  // run's --ask-timeout), as 90s, 20m or 2h.
  timeout?: string
}

export interface Answer {
  readonly text: string
  readonly turn: string
  readonly session: string
}

export interface CommandOptions {
  // Where it runs, relative to the project; by default the project.
  cwd?: string
  // How long it may take (default 10m).
  timeout?: string
}

export interface CommandResult {
  readonly ok: boolean
  // The exit code; -1 when it was killed, by its timeout or a signal.
  readonly code: number
  // What it printed, stdout and stderr together, its last 64 KiB.
  readonly output: string
}
