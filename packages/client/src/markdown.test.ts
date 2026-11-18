import { describe, expect, it } from 'vitest'
import { markdown } from './markdown'

describe('markdown', () => {
  // Whatever a reply holds, the page shows it as text: no markup but what
  // Markdown makes, and links to http and https only.
  it('keeps markup in a reply as text', () => {
    const html = markdown(
      [
        '<img src=x onerror=alert(1)> **<b>x</b>** `<i>`',
        '[a](javascript:alert(1)) [b](https://ok.example/"onmouseover="x) <javascript:alert(2)>',
        '[c](data:text/html,<script>alert(3)</script>) ![d](https://tracker.example/pixel.png)',
        '',
        '```',
        '</code><script>alert(4)</script>',
        '```',
      ].join('\n'),
    )
    expect(html).not.toMatch(/<img|<script|<b>|<i>|href="(javascript|data)|" ?on\w+=/)
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;')
    expect(html).toContain('[a](javascript:alert(1))')
    expect(html).toContain('<pre><code>&lt;/code&gt;&lt;script&gt;alert(4)&lt;/script&gt;\n</code></pre>')
    expect(html).toContain('!<a href="https://tracker.example/pixel.png"')
  })

  it('renders what agents write: steps with code, tables, quotes and links', () => {
    expect(markdown('1. Build it:\n   ```sh\n   go build\n   ```\n2. Run it')).toBe(
      '<ol>\n<li>Build it:<pre><code class="language-sh">go build\n</code></pre>\n</li>\n<li>Run it</li>\n</ol>\n',
    )
    expect(markdown('3. third\n4. fourth')).toContain('<ol start="3">')
    expect(markdown('| a | b |\n| --- | ---: |\n| 1 | 2 |')).toMatch(/^<div class="table"><table>\n<thead>/)
    expect(markdown('Tom &amp; Jerry, \\*not italic\\*')).toBe('<p>Tom &amp; Jerry, *not italic*</p>\n')
    expect(markdown('See https://example.com/a?b=1&c=2. Or README.md')).toBe(
      '<p>See <a href="https://example.com/a?b=1&amp;c=2" target="_blank" rel="noopener noreferrer">https://example.com/a?b=1&amp;c=2</a>. Or README.md</p>\n',
    )
    expect(markdown('line one\nline two')).toBe('<p>line one<br>\nline two</p>\n')
  })
})
