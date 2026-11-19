import { register as registerAcrylic } from '@ax-design/acrylic'
import { registerElevationLayer } from '@ax-design/elevation'
import { type AxNavigation, type AxNavigationItem, register as registerNavigation } from '@ax-design/navigation'
import { register as registerPressure } from '@ax-design/pressure'
import { registerRevealLayer } from '@ax-design/reveal-highlight'
import { register as registerSurface } from '@ax-design/surface'
import { register as registerTile } from '@ax-design/tile'
import type { ComponentChildren, HTMLAttributes } from 'preact'
import { useLayoutEffect, useRef } from 'preact/hooks'

// The page draws with the Axiom Design System: surfaces whose shapes,
// layers and tilt Axiom paints (the acrylic of a bar, the shadow of what
// floats, the tail of a prompt), the press of what the person taps, the
// indicator of what they chose, and tiles that start what they name.

/**
 * Whether the device points with a mouse. Only then does Reveal light what
 * the pointer comes near: on a touch screen it would flash under a finger,
 * and follow it across a list as it scrolls.
 */
export const reveals = typeof matchMedia === 'function' && matchMedia('(hover: hover) and (pointer: fine)').matches

/** The layers of a surface the person presses, with Reveal's light where the device has one. */
export const lit = (layers: string) => (reveals ? `${layers} reveal` : layers)

// registerAxiom defines the elements and layers the page uses. It runs
// before the page renders: Preact sets what an element defines as a
// property, and anything else as an attribute.
export function registerAxiom() {
  registerSurface()
  registerPressure()
  registerElevationLayer()
  registerAcrylic()
  registerNavigation()
  registerTile()
  if (reveals) registerRevealLayer()
}

// Navigation is a row of what the person chooses between, the indicator
// under the one chosen moving to another as it is chosen, by a tap or by
// the page. The items keep their own controls and the states they say.
export function Navigation({ at, children }: { at: number; children: ComponentChildren }) {
  const nav = useRef<AxNavigation>(null)
  useLayoutEffect(() => {
    const item = nav.current?.querySelectorAll<AxNavigationItem>(':scope > ax-navigation-item')[at]
    if (item) nav.current?.select(item)
  }, [at])
  return (
    <ax-navigation ref={nav} class="navigation" mode="none" horizontal>
      {children}
    </ax-navigation>
  )
}

type Axiom = HTMLAttributes<HTMLElement>

// Surface is a box Axiom paints its layers on, under what it holds; Tile is
// one that starts what it names, pressed before its action runs. Preact
// gives an element as a property whatever it has one of, and a surface
// keeps its layers in a field of that name: the layers go to it as its
// attribute instead, by a name it has no property of, which the document
// lowercases to the attribute's.
export const Surface = ({ layers, ...rest }: Axiom & { layers: string }) => (
  <ax-surface {...rest} Layers={layers} onDragStartCapture={native} />
)
export const Tile = ({ layers, ...rest }: Axiom & { layers: string }) => (
  <ax-tile {...rest} Layers={layers} onDragStartCapture={native} />
)

// native lets the browser drag a link out of a surface, as anywhere: a
// surface cancels the drags that reach it, so the page's surfaces stop them
// on their way in, before they come back out to it.
export const native = (event: DragEvent) => event.stopPropagation()

// Lamp is Reveal's light over what the person presses, where the device
// points with a mouse: it follows the pointer, and leans with a press.
export const Lamp = () => (reveals ? <Surface class="lamp" layers="reveal" aria-hidden="true" /> : null)

declare module 'preact' {
  namespace JSX {
    interface IntrinsicElements {
      'ax-surface': Axiom & { Layers: string }
      'ax-pressure': Axiom
      'ax-tile': Axiom & { Layers: string }
      'ax-navigation': HTMLAttributes<AxNavigation> & { mode?: 'links' | 'tabs' | 'none'; horizontal?: boolean }
      'ax-navigation-item': Axiom & { default?: boolean }
    }
  }
}
