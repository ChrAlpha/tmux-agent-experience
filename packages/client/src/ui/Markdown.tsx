import { useMemo } from 'preact/hooks'
import { markdown } from '../markdown'
import { cls } from './explain'

export function Markdown({ src, class: name }: { src: string; class?: string }) {
  const html = useMemo(() => markdown(src), [src])
  // biome-ignore lint/security/noDangerouslySetInnerHtml: markdown-it, with HTML off, escapes all the text it is given
  return <div class={cls('md', name)} dangerouslySetInnerHTML={{ __html: html }} />
}
