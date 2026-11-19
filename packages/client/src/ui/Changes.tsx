import { useEffect, useState } from 'preact/hooks'
import type { Computer } from '../computer'
import type { Changed, Session } from '../contract'
import { anchor, type Comment, commentable, lines, prompt, where } from '../patch'
import { cls, explain } from './explain'

// Changes shows what a session's directory changed against HEAD, each file's
// patch on request, and what the person says of its lines, which goes into
// the session's message as `path:LINE: comment` lines, each with the line
// it is about quoted. The person writes one comment at a time; it is kept
// in the tab until it is added or cancelled, whatever becomes of its file.
export function Changes({ c, id, s, put }: { c: Computer; id: string; s: Session | undefined; put: () => void }) {
  const read = c.changesOf(id).value
  const comments = c.commentsOf(id)
  const writing = c.writingOf(id).value
  const [open, setOpen] = useState<string | null>(writing?.file ?? null)
  if (!read.value) {
    return (
      <p class={cls('empty', read.error && 'error')}>
        {read.error ? explain(read.error) : 'Reading what its directory changed…'}
      </p>
    )
  }
  const { top, files, truncated } = read.value
  const said = comments.value
  // A comment being written shows on its line where its file's patch is
  // open, and here otherwise.
  const apart = writing && (open !== writing.file || !files.some((f) => f.path === writing.file))
  return (
    <div class="changes">
      {read.error && <Stale error={read.error} again={() => c.loadChanges(id)} />}
      <p class="muted top">{top}</p>
      {apart && <Writing c={c} id={id} w={writing} />}
      {!files.length && <p class="empty">Nothing changed against HEAD.</p>}
      <ul class="files">
        {files.map((f) => (
          <li key={`${f.status}/${f.path}`}>
            <button
              class="file"
              type="button"
              aria-expanded={open === f.path}
              onClick={() => setOpen(open === f.path ? null : f.path)}
            >
              <span class={cls('status', kind(f.status))}>{f.status}</span>
              <span class="path">{f.path}</span>
              <span class="count">{count(f)}</span>
            </button>
            {open === f.path && <Patch c={c} id={id} s={s} file={f.path} />}
          </li>
        ))}
      </ul>
      {truncated && <p class="muted">tmag lists no more of them.</p>}
      {said.length > 0 && (
        <div class="put">
          <span class="count">
            {said.length} comment{said.length === 1 ? '' : 's'}
          </span>
          <button class="link" type="button" onClick={() => (comments.value = [])}>
            Discard
          </button>
          <button
            class="primary"
            type="button"
            onClick={() => {
              const draft = c.draftOf(id)
              const rowsOf = (file: string) => lines(c.patchOf(id, file).value.value ?? '')
              const note = prompt(said, rowsOf)
              draft.value = draft.value ? `${draft.value}\n\n${note}` : note
              comments.value = []
              put()
            }}
          >
            Put in the message
          </button>
        </div>
      )}
    </div>
  )
}

// kind is how git says a file changed: added, deleted, not known to it, or
// otherwise modified.
function kind(status: string): string {
  if (status.includes('?')) return 'untracked'
  if (status.includes('A')) return 'added'
  if (status.includes('D')) return 'deleted'
  return 'modified'
}

// count says how a file changed: its lines added and removed, or why tmag
// counted none.
function count(f: Changed): string {
  if (f.filtered) return 'filtered'
  if (f.binary) return 'binary'
  return f.added === undefined ? '' : `+${f.added} −${f.removed ?? 0}`
}

// Stale says that what shows could not be read again, and reads it again.
function Stale({ error, again }: { error: Error; again: () => void }) {
  return (
    <p class="stale" role="status">
      Not read again: {explain(error)} It shows what was read before.{' '}
      <button class="link" type="button" onClick={again}>
        Read again
      </button>
    </p>
  )
}

// Patch is one file's patch, read when it opens and again whenever the watch
// reports the session, with the comments said of its lines. A comment stays
// with the line it is about as the patch changes under it; one whose line
// cannot be told apart in it any more shows at the patch's end, with the
// line quoted. While a comment is being written, and while the patch could
// not be read again, no other is begun.
function Patch({ c, id, s, file }: { c: Computer; id: string; s: Session | undefined; file: string }) {
  // biome-ignore lint/correctness/useExhaustiveDependencies: each report of the session reads the patch again
  useEffect(() => c.loadPatch(id, file), [c, id, file, s])
  const read = c.patchOf(id, file).value
  const comments = c.commentsOf(id)
  const writing = c.writingOf(id)
  const mine = writing.value?.file === file ? writing.value : null
  if (read.value === undefined) {
    return (
      <>
        <p class={cls('muted', read.error && 'error')}>{read.error ? explain(read.error) : 'Reading its patch…'}</p>
        {mine && <Writing c={c} id={id} w={mine} />}
      </>
    )
  }
  const rows = lines(read.value)
  const placed = new Map<number, Comment[]>()
  const moved: Comment[] = []
  for (const x of comments.value) {
    if (x.file !== file) continue
    const i = where(rows, x)
    if (i < 0) moved.push(x)
    else placed.set(i, [...(placed.get(i) ?? []), x])
  }
  const editing = mine ? where(rows, mine) : -1
  const begins = !writing.value && !read.error
  return (
    <div class="patch">
      {read.error && <Stale error={read.error} again={() => c.loadPatch(id, file)} />}
      {rows.map((l, i) => {
        const a = begins ? anchor(rows, i) : undefined
        const numbered = commentable(l)
        return (
          // biome-ignore lint/suspicious/noArrayIndexKey: a patch's lines are its rows
          <div key={i}>
            {a ? (
              <button
                class={cls('line', l.kind)}
                type="button"
                onClick={() => (writing.value = { ...a, file, text: '' })}
              >
                <span class="no">{l.old ?? ''}</span>
                <span class="no">{l.new ?? ''}</span>
                <span class="code">{l.text}</span>
              </button>
            ) : (
              <div class={cls('line', l.kind)}>
                {numbered && (
                  <>
                    <span class="no">{l.old ?? ''}</span>
                    <span class="no">{l.new ?? ''}</span>
                  </>
                )}
                <span class="code">{l.text}</span>
              </div>
            )}
            {placed.get(i)?.map((x, j) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: a line's comments only grow at its end
              <p key={j} class="comment">
                {x.text}
              </p>
            ))}
            {editing === i && <Say c={c} id={id} />}
          </div>
        )
      })}
      {(moved.length > 0 || (mine && editing < 0)) && (
        <div class="moved">
          <p class="muted">Said of lines that changed since:</p>
          {moved.map((x, j) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: comments only grow at their end
            <p key={j} class="comment">
              <code>{x.code}</code> {x.text}
            </p>
          ))}
          {mine && editing < 0 && (
            <>
              <p class="comment">
                <code>{mine.code}</code>
              </p>
              <Say c={c} id={id} />
            </>
          )}
        </div>
      )}
    </div>
  )
}

// Writing is the comment being written where its line does not show: the
// file and line it is about, the line quoted, and the box it is written in.
function Writing({ c, id, w }: { c: Computer; id: string; w: Comment }) {
  return (
    <div class="moved">
      <p class="muted">
        Writing on {w.file}:{w.line}
      </p>
      <p class="comment">
        <code>{w.code}</code>
      </p>
      <Say c={c} id={id} />
    </div>
  )
}

// Say is the box the comment being written is written in: Add keeps it with
// the others, Cancel drops it.
function Say({ c, id }: { c: Computer; id: string }) {
  const writing = c.writingOf(id)
  const comments = c.commentsOf(id)
  const w = writing.value
  if (!w) return null
  const done = (add: boolean) => {
    if (add && w.text.trim()) comments.value = [...comments.value, w]
    writing.value = null
  }
  return (
    <form
      class="say"
      onSubmit={(e) => {
        e.preventDefault()
        done(true)
      }}
    >
      <textarea
        aria-label="Comment"
        rows={2}
        placeholder="What to say of this line"
        value={w.text}
        onInput={(e) => (writing.value = { ...w, text: e.currentTarget.value })}
      />
      <button type="button" onClick={() => done(false)}>
        Cancel
      </button>
      <button class="primary" type="submit">
        Add
      </button>
    </form>
  )
}
