import { readFileSync } from 'node:fs'
import { defineConfig, type Plugin } from 'vitest/config'

// The version the page says it is, as package.json names it.
const { version } = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8')) as { version: string }

// Where the development and preview servers find tmag serve, and other
// computers, each `name=url` in TMAG_COMPUTERS. They proxy /v2/ to the one
// and /NAME/v2/ to the other's /v2/ as tailscale serve does: the Host kept,
// the path joined to the target.
const serve = process.env.TMAG_SERVE ?? 'http://127.0.0.1:7777'
const proxy = {
  '/v2': serve,
  ...Object.fromEntries(
    (process.env.TMAG_COMPUTERS ?? '')
      .split(',')
      .filter(Boolean)
      .map((pair) => {
        const [name = '', target = ''] = pair.split('=')
        return [`/${name}/v2`, { target, rewrite: (path: string) => path.slice(name.length + 1) }]
      }),
  ),
}

// The constant stylesheets Axiom's elements put in their shadow roots, each
// allowed by its hash, which holds for the versions package.json pins: the
// acrylic layer's, the navigation's, its indicator's detector, a
// navigation item's, and a tile's; and, in a browser without constructed
// stylesheets or CSS.registerProperty, a surface's own and the detector
// it then uses. The end-to-end tests fail on any stylesheet the policy
// refuses.
const axiom = [
  "'sha256-8jf9fEeH2e8ZSPd6+YIlhTdMwDA1WGV8mU3ONNkGyI8='",
  "'sha256-rwGMWuEA0NIobo1rd/io3PNUpZL0UQw1NMgq2cfK5xU='",
  "'sha256-1WO5Qs+UslVn1xujKbPy98vCvN+NB6TrGM20LrT8yCs='",
  "'sha256-B9xW0LcdEH9XluCgP45ZwPxkzrFMbrej84Yc6txuXrQ='",
  "'sha256-CihQqbJ6c4lonTCVNUT3cMlsCgCQPUJf0M8jKJch/Uc='",
  "'sha256-C11pwEak+gMp9Jv+4icmt/kTCIinZzGQrU7pj/vBthE='",
  "'sha256-x5d1M/YJ0WqrLg63IlQZFRAF0bcHnPwqeUNlPDUPUQs='",
]

// The built page loads nothing but its own files: data: images are the
// noise of acrylic, drawn in the page, not fetched. The development server
// injects styles inline, so only the build carries the policy.
const policy = [
  "default-src 'none'",
  "script-src 'self'",
  `style-src 'self' ${axiom.join(' ')}`,
  "connect-src 'self'",
  "img-src 'self' data:",
  "base-uri 'none'",
  "form-action 'none'",
].join('; ')

const contentSecurityPolicy: Plugin = {
  name: 'content-security-policy',
  apply: 'build',
  transformIndexHtml: () => [
    { tag: 'meta', attrs: { 'http-equiv': 'Content-Security-Policy', content: policy }, injectTo: 'head-prepend' },
  ],
}

export default defineConfig({
  plugins: [contentSecurityPolicy],
  define: { 'import.meta.env.VERSION': JSON.stringify(version) },
  oxc: { jsx: { runtime: 'automatic', importSource: 'preact' } },
  server: { proxy },
  preview: { proxy },
  test: { include: ['src/**/*.test.ts'] },
})
