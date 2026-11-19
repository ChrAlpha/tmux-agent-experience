import { useEffect, useState } from 'preact/hooks'
import { Failure } from '../api'
import type { Computer } from '../computer'
import { expand, tilde, where } from '../places'
import { cls, explain } from './explain'
import { Branch, Folder, Up } from './icons'

// A path as the person types it, read into the directory whose
// subdirectories it completes and the start of the name it completes.
function completing(path: string): { parent: string; prefix: string } | null {
  if (!path.startsWith('/')) return null
  const cut = path.lastIndexOf('/')
  return { parent: path.slice(0, cut) || '/', prefix: path.slice(cut + 1) }
}

const join = (dir: string, name: string) => `${dir === '/' ? '' : dir}/${name}`

/** How many of the directories a path completes in the picker shows. */
const shownAtOnce = 60

// missing says whether the computer said a directory is none.
export const missing = (c: Computer, dir: string) => {
  const e = c.dirOf(dir).value.error
  return e instanceof Failure && e.code === 'not_found'
}

/** The directory a path the person typed names: absolute, with no slash at its end. */
export const chosen = (typed: string, home?: string) => expand(typed, home).replace(/(.)\/+$/, '$1')

// DirPicker is where the person chooses the directory an agent starts in:
// the places sessions ran in lately, and a path that completes from the
// directories in it as it is typed, where the computer lists them; else
// the directories its sessions and panes run in now. A place picked from a
// list goes to onChoose too.
export function DirPicker({
  c,
  value,
  onChange,
  onChoose,
  open,
  setOpen,
}: {
  c: Computer
  value: string
  onChange: (typed: string) => void
  onChoose: (dir: string) => void
  open: boolean
  setOpen: (open: boolean) => void
}) {
  const lists = c.can('dirs')
  // Until the person types, the picker offers the places; once they type,
  // the directories their path completes in.
  const [typing, setTyping] = useState(false)
  const places = c.places.value.value
  const home = places?.home
  const path = chosen(value, home)
  // Where the computer lists directories, the places are read as the
  // picker opens; the directory the path completes in, as it is typed.
  useEffect(() => {
    if (lists && !places) c.loadPlaces()
  }, [c, lists, places])
  const typed = typing || value.endsWith('/') ? completing(expand(value, home)) : null
  const parent = typed?.parent
  useEffect(() => {
    if (lists && open && parent) c.loadDir(parent)
  }, [c, lists, open, parent])
  // The directory chosen is read too, for the repository it is in.
  useEffect(() => {
    if (!lists || !path.startsWith('/')) return
    const read = setTimeout(() => c.loadDir(path), 300)
    return () => clearTimeout(read)
  }, [c, lists, path])

  const repo = path.startsWith('/') ? c.repoAt(path) : undefined
  const w = path.startsWith('/') ? where(path, repo) : undefined
  if (!open) {
    return (
      <button class="picked" type="button" aria-label={`Directory ${path}, change it`} onClick={() => setOpen(true)}>
        <Folder />
        <span class="body">
          <span class="line1">
            <span class="name">{w?.name || 'Choose a directory'}</span>
            {w?.branch && (
              <span class={cls('chip', w.worktree && 'tree')}>
                <Branch />
                {w.branch}
              </span>
            )}
          </span>
          <span class="meta mono">{tilde(path, home)}</span>
        </span>
        <span class="go">Change</span>
      </button>
    )
  }

  const known = lists
    ? (places?.recent ?? []).map((p) => p.dir)
    : [
        ...c.view.value.panes.flatMap((p) => (p.cwd ? [p.cwd] : [])),
        ...[...c.view.value.sessions.values()].flatMap((s) => (s.cwd ? [s.cwd] : [])),
      ]
  const start = typing ? expand(value, home) : ''
  const recent = [...new Set(known)].filter((d) => !start || d.startsWith(start)).slice(0, 8)
  const listing = parent ? c.dirOf(parent).value : undefined
  const hidden = typed?.prefix.startsWith('.')
  const subdirs = (listing?.value?.dirs ?? []).filter(
    (d) => d.name.toLowerCase().startsWith((typed?.prefix ?? '').toLowerCase()) && (hidden || !d.name.startsWith('.')),
  )
  const pick = (dir: string) => {
    onChange(tilde(dir, home))
    onChoose(chosen(dir, home))
  }
  return (
    <div class="picker">
      <input
        class="path"
        aria-label="Directory"
        placeholder={home ? '~/path/to/project' : '/path/to/project'}
        autocapitalize="none"
        autocomplete="off"
        spellcheck={false}
        value={value}
        onInput={(e) => {
          setTyping(true)
          onChange(e.currentTarget.value)
        }}
      />
      {lists && parent && (
        <div class="browse">
          <div class="where">
            <span class="mono">{tilde(parent, home)}</span>
            {parent !== '/' && (
              <button
                class="icon-button"
                type="button"
                aria-label="Up one level"
                onClick={() => pick(`${parent.slice(0, parent.lastIndexOf('/')) || '/'}/`.replace('//', '/'))}
              >
                <Up />
              </button>
            )}
          </div>
          {listing?.error && <p class="muted error">{explain(listing.error)}</p>}
          <ul class="dirs">
            {subdirs.slice(0, shownAtOnce).map((d) => (
              <li key={d.name}>
                <button type="button" class="dir" onClick={() => pick(`${join(parent, d.name)}/`)}>
                  <Folder />
                  <span class="name">{d.name}</span>
                  {d.repo && <span class="chip">git</span>}
                </button>
              </li>
            ))}
            {subdirs.length > shownAtOnce && (
              <li class="muted none">{subdirs.length - shownAtOnce} more: type more of the name.</li>
            )}
            {!!subdirs.length && listing?.value?.truncated && (
              <li class="muted none">tmag lists only part of a directory this large: type more of the name.</li>
            )}
            {listing?.value && !subdirs.length && (
              <li class="muted none">
                {listing.value.truncated
                  ? 'Nothing tmag lists here starts so, and it lists only part of a directory this large. Type the whole name.'
                  : 'No directory here starts so.'}
              </li>
            )}
          </ul>
        </div>
      )}
      {recent.length > 0 && (
        <div class="recent">
          <span class="label">{lists ? 'Recent' : 'Where its sessions run'}</span>
          <ul class="dirs">
            {recent.map((d) => (
              <li key={d}>
                <button type="button" class="dir" onClick={() => pick(d)}>
                  <Folder />
                  <span class="name">{where(d, c.repoAt(d)).name}</span>
                  <span class="meta mono">{tilde(d, home)}</span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
      {lists && missing(c, path) ? (
        <p class="muted">No directory has this path.</p>
      ) : (
        path.startsWith('/') && (
          <button class="done" type="button" onClick={() => setOpen(false)}>
            Use this directory
          </button>
        )
      )}
    </div>
  )
}
