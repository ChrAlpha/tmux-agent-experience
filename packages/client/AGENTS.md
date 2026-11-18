# AGENTS.md

This repository is a page for a phone: Preact components in TypeScript that
Vite builds into static files, which `tailscale serve`, or any server that also
proxies `/v2/` to `tmag serve`, serves as they are. README.md says what it shows
and how to set it up.

## Promises

- **The page is its files.** No server code, and no request beyond its own
  origin: the Content-Security-Policy that `vite.config.ts` writes into the
  built page holds it to that. Every dependency runs with a credential that
  can make agents run commands on the owner's computer: one is added only for
  what the page cannot do well without it, and the list in `package.json`
  stays short.
- **It speaks one contract.** Only `tmag/http/v2`, as tmux-agent-experience's
  `docs/http-api.md` documents it, and the `tmag/v2` documents it relays;
  `src/contract.ts` types what the page reads of them. A field the contract
  does not name is not there; one it may omit is optional. A change the page
  needs from tmag is a change to that contract, made there first.
- **The watch is the truth.** `src/view.ts` rebuilds sessions and panes from
  `/v2/watch` as the contract tells a client to, the rules of
  tmux-agent-experience's e2e follower. What the page reads on demand (turns,
  a request, a screen) it reads again when an event reports that session, and
  what changes unreported on its own while it shows: a screen every two
  seconds, and turns every second, three times, while they lag what tmag
  reported. A control action whose answer is lost is an unknown outcome: the
  page says so and never tries again by itself.
- **What an agent wrote stays text.** Components render strings as text.
  Only `src/ui/Markdown.tsx` sets HTML, from `src/markdown.ts`: markdown-it
  with raw HTML off, links to http and https only, no images.
- **The page keeps its credential and drafts, nothing else.** The credential
  lives in the browser's storage, goes only in the `Authorization` header, and
  never in a URL or a log. Turns, requests and screens are read when shown.
- **Out of scope, by decision:** starting agents (an agent starts its peers
  through tmux), browsing files or diffs, several computers, push of its own
  (`@tmag-notify` and ntfy carry alerts), seen state. Changing that is the
  owner's decision, not an agent's.

## Layout

| Path | Role |
| --- | --- |
| `index.html`, `src/main.tsx` | The document, and the page fitted to the visual viewport |
| `public/` | tmag's icon, as tmux-agent-experience's landing page has it, and the PNG iOS needs of it |
| `vite.config.ts` | The build, its Content-Security-Policy, the `/v2/` proxy of `dev` and `preview`, and Vitest |
| `src/contract.ts` | The documents the page reads and writes |
| `src/api.ts` | Requests, pairing, and the event stream |
| `src/view.ts` | The view rebuilt from watch documents, and how a session reads |
| `src/store.ts` | Signals the screens read: credential, route, the watch's lifecycle, what a session shows |
| `src/markdown.ts` | Markdown to HTML that holds only Markdown's tags |
| `src/ui/` | Screens: pairing, agents, a session with its turns, screen, requests and composer |
| `src/app.css` | Styles, light and dark, for phones first |
| `src/*.test.ts` | Vitest tests of the modules that need no browser |
| `e2e/` | Playwright tests against tmag and fake agents in a private tmux server |
| `.github/workflows/` | Checks and end-to-end tests; releases from `v*` tags |

## Checks

```sh
pnpm check
TMAG_SRC=/path/to/tmux-agent-experience pnpm e2e
```

`pnpm e2e` runs every test in WebKit and Chromium at a phone's size. A change
to what the page shows comes with a test there; look at it in a browser too,
light and dark. Never point a test at the developer's tmux server or
credentials: `e2e/world.ts` builds its own.

## Conventions

Commits follow Conventional Commits. Code comments and docs are in English.
Biome formats and lints; `pnpm format` applies it. The version lives in
`package.json`, and a `vX.Y.Z` tag that matches it releases the built page.
README.md's minimum tmag, `TMAG_REF` in `.github/workflows/ci.yml` and
`minimum` in `src/contract.ts` move together; a test keeps them so. Keep the
MIT license and its copyright.
