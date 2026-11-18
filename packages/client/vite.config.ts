import { defineConfig, type Plugin } from 'vitest/config'

// Where the development and preview servers find tmag serve. They proxy /v2/
// to it as tailscale serve does: the Host kept, the path joined to the target.
const serve = process.env.TMAG_SERVE ?? 'http://127.0.0.1:7777'

// The built page loads nothing but its own files. The development server
// injects styles inline, so only the build carries the policy.
const policy = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "connect-src 'self'",
  "img-src 'self'",
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
  oxc: { jsx: { runtime: 'automatic', importSource: 'preact' } },
  server: { proxy: { '/v2': serve } },
  preview: { proxy: { '/v2': serve } },
  test: { include: ['src/**/*.test.ts'] },
})
