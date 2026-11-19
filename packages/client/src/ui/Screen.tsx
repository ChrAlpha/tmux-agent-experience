import { useLayoutEffect } from 'preact/hooks'
import { Failure } from '../api'
import type { Computer } from '../computer'
import { explain } from './explain'
import { useFollow } from './Scroller'
import { Terminal } from './Terminal'

// Screen is what a pane shows, read when shown, in its terminal's colours.
// A screen that could not be read again shows as it was, and says so; one
// whose pane tmag no longer finds shows nothing.
export function Screen({ c, target }: { c: Computer; target: string }) {
  const read = c.screenOf(target).value
  const follow = useFollow()
  // biome-ignore lint/correctness/useExhaustiveDependencies: the screen read changes what shows
  useLayoutEffect(follow, [follow, read])
  const gone = read.error instanceof Failure && read.error.code === 'not_found'
  if (!read.value || gone) return <pre class="terminal">{read.error ? explain(read.error) : 'Reading its screen…'}</pre>
  return (
    <>
      {read.error && (
        <p class="stale" role="status">
          Not read again: {explain(read.error)} It shows the screen as it was.
        </p>
      )}
      <Terminal text={read.value.text} ansi={read.value.ansi} />
    </>
  )
}
