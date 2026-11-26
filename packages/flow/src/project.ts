import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readdirSync, readFileSync, realpathSync, statSync, writeFileSync } from 'node:fs'
import { basename, dirname, extname, isAbsolute, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { UsageError } from './errors.ts'

// Where runs live: in the project's main worktree, under .tmag-flow, which
// ignores itself, so every worktree of a repository shares its runs and git
// sees none of them.

export const runsDirName = '.tmag-flow'

// projectOf is the main worktree of the repository dir is in, or dir itself
// outside one.
export function projectOf(dir: string): string {
  try {
    const common = execFileSync('git', ['-C', dir, 'rev-parse', '--path-format=absolute', '--git-common-dir'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
    if (basename(common) === '.git') return realpathSync(dirname(common))
  } catch {
    // not in a repository
  }
  return realpathSync(dir)
}

export function runsDir(project: string): string {
  const dir = join(project, runsDirName)
  mkdirSync(dir, { recursive: true })
  const ignore = join(dir, '.gitignore')
  if (!existsSync(ignore)) writeFileSync(ignore, '*\n')
  return dir
}

const runName = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

export function checkRunName(name: string): void {
  if (!runName.test(name)) {
    throw new UsageError(
      `a run is named by up to 64 letters, digits, ., - and _, starting with a letter or digit; not ${JSON.stringify(name)}`,
    )
  }
}

// newRun names a run of plan started now, unless taken.
export function newRun(project: string, plan: string, name?: string): { name: string; dir: string } {
  const root = runsDir(project)
  if (name !== undefined) {
    checkRunName(name)
    const dir = join(root, name)
    if (existsSync(dir)) {
      throw new UsageError(
        `run ${name} exists already`,
        `resume it with tmag-flow resume ${name}, or name another with --run`,
      )
    }
    mkdirSync(dir)
    return { name, dir }
  }
  const stamp = new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15)
  for (let i = 1; ; i++) {
    const candidate = `${plan}-${stamp}${i === 1 ? '' : `-${i}`}`
    const dir = join(root, candidate)
    try {
      mkdirSync(dir)
      return { name: candidate, dir }
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'EEXIST') throw e
    }
  }
}

// findRun is the directory of a run named in this project, or given by its
// path.
export function findRun(project: string, run: string): string {
  const dir = isAbsolute(run) || run.includes('/') ? resolve(run) : join(project, runsDirName, run)
  if (!existsSync(join(dir, 'journal.jsonl'))) {
    throw new UsageError(
      `no run ${run} in ${join(project, runsDirName)}`,
      'tmag-flow list shows the runs of this project',
    )
  }
  return dir
}

// runs lists the runs of a project, most recently changed first.
export function runs(project: string): { name: string; dir: string; changed: number }[] {
  const root = join(project, runsDirName)
  if (!existsSync(root)) return []
  return readdirSync(root, { withFileTypes: true })
    .filter((e) => e.isDirectory() && existsSync(join(root, e.name, 'journal.jsonl')))
    .map((e) => ({
      name: e.name,
      dir: join(root, e.name),
      changed: statSync(join(root, e.name, 'journal.jsonl')).mtimeMs,
    }))
    .sort((a, b) => b.changed - a.changed)
}

// The plans that ship with tmag-flow, beside this module: TypeScript in a
// checkout, JavaScript in the package.
const bundledDir = fileURLToPath(new URL('../plans/', import.meta.url))

export function bundledPlans(): string[] {
  if (!existsSync(bundledDir)) return []
  return readdirSync(bundledDir)
    .filter((f) => /\.(ts|js)$/.test(f) && !f.endsWith('.d.ts'))
    .map((f) => f.replace(/\.(ts|js)$/, ''))
    .sort()
}

export interface PlanFile {
  name: string
  path: string
  hash: string
}

// resolvePlan finds a plan: one that ships with tmag-flow by its name, or a
// file by its path.
export function resolvePlan(plan: string, cwd: string): PlanFile {
  let path: string | undefined
  if (/^[a-z][a-z0-9-]*$/.test(plan)) {
    path = ['.js', '.ts'].map((ext) => join(bundledDir, plan + ext)).find((p) => existsSync(p))
    if (!path && !existsSync(resolve(cwd, plan))) {
      throw new UsageError(
        `no plan ${plan}: the plans that ship are ${bundledPlans().join(', ')}`,
        'name one of them, or give the path of a plan file',
      )
    }
  }
  path ??= resolve(cwd, plan)
  if (!existsSync(path) || !statSync(path).isFile()) throw new UsageError(`no plan file ${path}`)
  const name =
    basename(path, extname(path))
      .replace(/[^A-Za-z0-9-]+/g, '-')
      .replace(/^-+|-+$/g, '') || 'plan'
  return { name, path: realpathSync(path), hash: hashFile(path) }
}

export function hashFile(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex')
}

export function hash(...parts: unknown[]): string {
  return createHash('sha256').update(JSON.stringify(parts)).digest('hex').slice(0, 16)
}
