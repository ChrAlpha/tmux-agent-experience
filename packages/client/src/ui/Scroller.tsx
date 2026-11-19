import type { ComponentChildren } from 'preact'
import { createContext } from 'preact'
import { useCallback, useContext, useLayoutEffect, useRef } from 'preact/hooks'

// Follow keeps a scroller at its end when what it shows changes, as long as
// the person has not scrolled away from it, or brings it there.
const Follow = createContext((_pin?: boolean) => {})

export const useFollow = () => useContext(Follow)

export function Scroller({ children }: { children: ComponentChildren }) {
  const ref = useRef<HTMLElement>(null)
  const stick = useRef(true)
  const follow = useCallback((pin = false) => {
    const el = ref.current
    if (pin) stick.current = true
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [])
  useLayoutEffect(() => {
    // A keyboard that opens makes the scroller shorter.
    const el = ref.current
    if (!el) return
    const resized = new ResizeObserver(() => follow())
    resized.observe(el)
    return () => resized.disconnect()
  }, [follow])
  return (
    <main
      class="scroll"
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 120
      }}
    >
      <Follow.Provider value={follow}>{children}</Follow.Provider>
    </main>
  )
}
