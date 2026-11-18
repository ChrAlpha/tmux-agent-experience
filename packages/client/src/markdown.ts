// The Markdown agents write in replies and plans, rendered by markdown-it to
// HTML that holds only the tags Markdown makes: raw HTML in the text stays
// text, links go to http and https only, and images, which the page's policy
// would not load, show as links.

import MarkdownIt from 'markdown-it'

const md = new MarkdownIt({ html: false, linkify: true, breaks: true })

// Bare addresses become links only with their scheme or www., so that
// README.md or main.go stay words.
md.linkify.set({ fuzzyLink: false, fuzzyEmail: false })
md.validateLink = (url) => /^https?:\/\//i.test(url)
md.disable('image')

md.renderer.rules.link_open = (tokens, i, options, _env, self) => {
  tokens[i]?.attrSet('target', '_blank')
  tokens[i]?.attrSet('rel', 'noopener noreferrer')
  return self.renderToken(tokens, i, options)
}
// A wide table scrolls on its own instead of widening the page.
md.renderer.rules.table_open = () => '<div class="table"><table>\n'
md.renderer.rules.table_close = () => '</table></div>\n'

export const markdown = (src: string): string => md.render(src)
