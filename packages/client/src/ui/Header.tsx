import type { ComponentChildren } from 'preact'
import { back } from '../store'
import { Lamp, native, Surface } from './axiom'
import { cls } from './explain'
import { Back } from './icons'

// Header is the bar a screen opens with, acrylic over what scrolls: the way
// back, what the screen shows, what the person may do there, and what they
// choose between under it.
export function Header({
  title,
  meta,
  children,
  actions,
  below,
  up = true,
}: {
  title: ComponentChildren
  meta?: ComponentChildren
  children?: ComponentChildren
  actions?: ComponentChildren
  below?: ComponentChildren
  up?: boolean
}) {
  return (
    <Surface class="bar-top" layers="acrylic">
      <header class="head">
        <div class="head-row">
          {up && (
            <Round label="Back" class="back" onClick={back}>
              <Back />
            </Round>
          )}
          <div class="what">
            <h1 class="name">{title}</h1>
            {meta && <div class="meta">{meta}</div>}
            {children}
          </div>
          {actions && <div class="actions-top">{actions}</div>}
        </div>
        {below}
      </header>
    </Surface>
  )
}

// Round is a button of a bar, as Windows draws an app bar's: its glyph in a
// ring, leaning away from where it is pressed. With an address, it is a link.
export function Round({
  label,
  href,
  onClick,
  class: name,
  children,
}: {
  label: string
  href?: string
  onClick?: () => void
  class?: string
  children: ComponentChildren
}) {
  const ring = <span class="ring">{children}</span>
  return (
    <ax-pressure class="press" onDragStartCapture={native}>
      <Lamp />
      {href === undefined ? (
        <button class={cls('round', name)} type="button" aria-label={label} onClick={onClick}>
          {ring}
        </button>
      ) : (
        <a class={cls('round', name)} href={href} aria-label={label} onClick={onClick}>
          {ring}
        </a>
      )}
    </ax-pressure>
  )
}
