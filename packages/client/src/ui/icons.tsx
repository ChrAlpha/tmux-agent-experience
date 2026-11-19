// The page's icons, in the manner of Segoe MDL2: thin strokes with square
// ends and mitred corners, in the colour of the text around them, drawn on
// a 24-unit grid, hidden from assistive technology, which reads the label of
// what holds them.

import type { ComponentChildren } from 'preact'

function Icon({ children, size = 20 }: { children: ComponentChildren; size?: number }) {
  return (
    <svg
      class="icon"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.5"
      stroke-linecap="square"
      stroke-linejoin="miter"
      aria-hidden="true"
    >
      {children}
    </svg>
  )
}

export const Back = () => (
  <Icon>
    <path d="M12 5.5L5.5 12l6.5 6.5M5.5 12h13" />
  </Icon>
)
export const Plus = () => (
  <Icon>
    <path d="M12 4v16M4 12h16" />
  </Icon>
)
export const Send = () => (
  <Icon>
    <path d="M3.5 4l17 8-17 8 2.5-8zM6 12h7" />
  </Icon>
)
export const Stop = () => (
  <Icon size={18}>
    <rect x="6" y="6" width="12" height="12" fill="currentColor" stroke="none" />
  </Icon>
)
export const Computers = () => (
  <Icon>
    <path d="M3 4.5h18v11H3zM8 19.5h8M12 15.5v4" />
  </Icon>
)
export const Folder = () => (
  <Icon size={18}>
    <path d="M3 5.5h6l2 2h10v11H3z" />
  </Icon>
)
export const Branch = () => (
  <Icon size={14}>
    <circle cx="6" cy="5.5" r="2" />
    <circle cx="6" cy="18.5" r="2" />
    <circle cx="18" cy="7.5" r="2" />
    <path d="M6 7.5v9M18 9.5c0 4.5-6 3.5-11 7" />
  </Icon>
)
export const Next = () => (
  <Icon size={16}>
    <path d="M9 5l7 7-7 7" />
  </Icon>
)
export const Up = () => (
  <Icon size={18}>
    <path d="M12 19.5V5M5.5 11.5L12 5l6.5 6.5" />
  </Icon>
)
export const Copy = () => (
  <Icon size={16}>
    <path d="M8.5 8.5h11v11h-11zM15.5 8.5v-4h-11v11h4" />
  </Icon>
)
