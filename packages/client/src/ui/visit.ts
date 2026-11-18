import { useLayoutEffect, useRef } from 'preact/hooks'

/** What an action the person began on a screen asks once it ends. */
export interface Visit {
  /** Whether the screen still shows: what it says of the action may show there. */
  shown(): boolean
  /** Whether the person is still where they began it, the screen showing at the same address: only then may it move them. */
  here(): boolean
}

// useVisit lets an action the person began on a screen tell, once it ends,
// whether its screen still shows and whether the person is still where they
// began it. One that ends after they left moves them nowhere. Calling what
// it returns begins an action.
export function useVisit(): () => Visit {
  const shown = useRef(true)
  useLayoutEffect(
    () => () => {
      shown.current = false
    },
    [],
  )
  return () => {
    const from = location.hash
    return { shown: () => shown.current, here: () => shown.current && location.hash === from }
  }
}
