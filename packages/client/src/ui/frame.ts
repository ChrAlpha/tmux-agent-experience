import { useCallback, useRef } from 'preact/hooks'

// useFrame keeps the heights of a screen's bar and of what stays at its
// foot in the screen's --head and --foot, which what scrolls between them
// is padded by: both lie over it, so that it passes under their acrylic.
// The room under the bar, in --room, is as tall as what stays at the foot
// may grow, as a keyboard shrinks the screen. It is the ref of the screen,
// whichever element the screen renders.
export function useFrame() {
  const stop = useRef<() => void>(undefined)
  return useCallback((screen: HTMLElement | null) => {
    stop.current?.()
    stop.current = undefined
    if (!screen) return
    const measure = () => {
      const height = (part: string) => screen.querySelector(`:scope > ${part}`)?.getBoundingClientRect().height ?? 0
      const head = height('.bar-top')
      screen.style.setProperty('--head', `${head}px`)
      screen.style.setProperty('--foot', `${height('.dock')}px`)
      screen.style.setProperty('--room', `${Math.max(0, screen.clientHeight - head - 8)}px`)
    }
    // A part that resized pads what scrolls in the next frame: padded in
    // the same one, what scrolls, beside the part, would resize where its
    // own observer could not be told of it until then.
    let frame = 0
    const sizes = new ResizeObserver(() => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(measure)
    })
    // The parts of a screen come and go with what it shows.
    const watch = () => {
      sizes.disconnect()
      sizes.observe(screen)
      for (const part of screen.querySelectorAll(':scope > .bar-top, :scope > .dock')) sizes.observe(part)
      measure()
    }
    const parts = new MutationObserver(watch)
    parts.observe(screen, { childList: true })
    watch()
    stop.current = () => {
      cancelAnimationFrame(frame)
      sizes.disconnect()
      parts.disconnect()
    }
  }, [])
}
