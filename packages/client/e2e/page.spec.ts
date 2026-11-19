import { execFileSync } from 'node:child_process'
import { appendFileSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Page, type Route, test } from '@playwright/test'
import type { Credential } from '../src/contract'
import {
  type Agent,
  type At,
  addTurn,
  agents,
  credential,
  pairingCode,
  paneOf,
  person,
  said,
  serves,
  session,
  startable,
  startServe,
  stopServe,
  tmux,
} from './world'

const { api, web, docs, ops } = agents

// Each test pairs anew, so tests run alone, and one after another: they
// share the agents.
test.describe.configure({ mode: 'serial' })

// paired gives the page a credential of its own on each computer, as pairing
// would have, before it first loads; what the page keeps after that it keeps.
async function paired(page: Page, ...at: At[]): Promise<Credential[]> {
  const all = await Promise.all((at.length ? at : ['' as At]).map(credential))
  const saved = all.map((credential, i) => ({ name: at[i] ?? '', credential }))
  await page.addInitScript((s) => {
    if (!localStorage.getItem('tmag.computers')) localStorage.setItem('tmag.computers', JSON.stringify(s))
  }, saved)
  return all
}

// infoAs answers a read of what a computer is with what it says, as doc
// changes it: the instance it names stays.
async function infoAs(route: Route, doc: Record<string, unknown>) {
  const res = await route.fetch()
  await route.fulfill({ response: res, json: { ...(await res.json()), ...doc } })
}

const open = (page: Page, a: Agent) => page.goto(`/#/${a.at ? `${a.at}/` : ''}s/${a.session}`)

/** An agent's row in a list, whatever other rows say of it. */
const row = (page: Page, a: Agent) => page.locator('li.row', { has: page.locator(`a[href$="s/${a.session}"]`) })

/** Opens an agent's session from its row. */
const tap = (page: Page, a: Agent) => row(page, a).locator('.row-link').click()

/** Goes back, as the page's own back does. */
const back = (page: Page) => page.getByRole('button', { name: 'Back' }).click()

/** The start sheet in the directory of an agent's session. */
const startIn = (page: Page, a: Agent, at = '') =>
  page.goto(`/#/${at ? `${at}/` : ''}start/${encodeURIComponent(session(a)?.cwd ?? '')}`)

/** The directory the start sheet chose, which the person may change. */
const picked = (page: Page) => page.getByRole('button', { name: /^Directory .*, change it$/ })

/** The computers this device works with, on their own page. */
async function computers(page: Page) {
  await page.getByRole('link', { name: 'Computers' }).click()
  await expect(page.getByRole('heading', { name: 'Computers' })).toBeVisible()
  return page.locator('.computer')
}

async function idle(a: Agent) {
  await expect.poll(() => session(a)?.state, { timeout: 15_000 }).toBe('idle')
}

async function prompt(page: Page, text: string) {
  await page.getByRole('textbox', { name: 'Message' }).fill(text)
  await page.getByRole('button', { name: 'Send' }).click()
}

// What the page's Content-Security-Policy refused while a test ran: a
// stylesheet of an Axiom element whose version moved from its hash, or
// anything the page should not load. Every test fails on one.
const refusals = new WeakMap<Page, string[]>()

test.beforeEach(({ page }) => {
  const refused: string[] = []
  refusals.set(page, refused)
  page.on('console', (m) => m.text().includes('Content Security Policy') && refused.push(m.text()))
})

test.afterEach(async ({ page }) => {
  // A read the page polls for may be in a test's route as it ends.
  await page.unrouteAll({ behavior: 'ignoreErrors' })
  for (const a of Object.values(agents)) await idle(a)
  expect(refusals.get(page)).toEqual([])
})

test('pairs with a code and opens the session it was sent to', async ({ page }) => {
  await open(page, api)
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible()
  await page.getByRole('textbox', { name: 'Code' }).fill('WRONGCODE')
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.getByRole('alert')).toHaveText('That code is wrong, used or expired: run tmag auth pair again.')

  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode().toLowerCase())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.locator('header .name')).toHaveText('api')
  await back(page)
  for (const name of ['api', 'web', 'docs']) await expect(page.locator('.row .name', { hasText: name })).toBeVisible()
  await expect(await computers(page)).toContainText('This device: e2e')
  // Its icons are its own.
  for (const icon of ['/favicon.svg', '/apple-touch-icon.png']) expect((await page.request.get(icon)).ok()).toBe(true)
})

test('says why a first pairing failed after the address changed meanwhile, and pairs again', async ({ page }) => {
  let go = () => {}
  const held = new Promise<void>((r) => {
    go = r
  })
  await page.route(
    (url) => url.pathname === '/v2/pair',
    async (route) => {
      await held
      await route.continue()
    },
  )
  await open(page, api)
  await page.getByRole('textbox', { name: 'Code' }).fill('WRONGCODE')
  await page.getByRole('button', { name: 'Pair' }).click()
  await page.goto('/#/')
  go()
  await expect(page.getByRole('alert')).toHaveText('That code is wrong, used or expired: run tmag auth pair again.')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(await computers(page)).toContainText('This device: e2e')
})

test('goes back where the person came from, as the browser would', async ({ page }) => {
  await paired(page)
  // A session opened from a link goes up to the sessions in its place.
  await open(page, web)
  await expect(page.locator('header .name')).toHaveText('web')
  const entries = await page.evaluate(() => history.length)
  await back(page)
  await expect(page.getByRole('heading', { name: 'Inbox' })).toBeVisible()
  expect(await page.evaluate(() => history.length)).toBe(entries)
  // One opened from the projects goes back to them, and forward to it again.
  await page.getByRole('link', { name: 'Projects' }).click()
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
  expect(await page.evaluate(() => history.length), 'the views take no entry of their own').toBe(entries)
  await tap(page, api)
  await expect(page.locator('header .name')).toHaveText('api')
  await back(page)
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
  await page.goForward()
  await expect(page.locator('header .name')).toHaveText('api')
  if (!serves('start')) return
  // What a session opens goes back to it, and it to the projects, across a reload.
  await page.getByRole('link', { name: 'New agent here' }).click()
  await expect(page.getByRole('heading', { name: 'New agent' })).toBeVisible()
  await page.reload()
  await back(page)
  await expect(page.locator('header .name')).toHaveText('api')
  await back(page)
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
})

test('finds a view where the person left it', async ({ page }) => {
  await paired(page)
  await page.setViewportSize({ width: 393, height: 320 })
  await page.goto('/#/projects')
  const list = page.locator('main.scroll')
  const last = page.locator('li.row .row-link').last()
  await expect(row(page, docs)).toBeVisible()
  await last.scrollIntoViewIfNeeded()
  const at = await list.evaluate((el) => el.scrollTop)
  expect(at).toBeGreaterThan(0)
  await last.click()
  await expect(page).toHaveURL(/#\/(\w+\/)?s\//)
  await back(page)
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
  await expect.poll(() => list.evaluate((el) => el.scrollTop)).toBe(at)
})

test("shows a session's turns, and what agents write as Markdown that stays text", async ({ page }) => {
  await paired(page)
  addTurn(api, {
    id: 'p-md',
    peer: `[tmag] From codex 0b000000, another agent.\n${Array.from({ length: 20 }, (_, i) => `step ${i}`).join('\n')}`,
    reply: [
      '## Build',
      '',
      '1. Build it:',
      '   ```sh',
      '   go build ./...',
      '   ```',
      '2. Check https://example.com/docs?a=1&b=2.',
      '',
      '| Package | Result |',
      '| --- | --- |',
      '| `internal/serve` | ok |',
      '',
      'Tom &amp; Jerry <img src=x onerror=alert(1)> <script>alert(2)</script> [x](javascript:alert(3))',
    ].join('\n'),
  })
  let dialogs = 0
  page.on('dialog', (d) => {
    dialogs++
    void d.dismiss()
  })
  await open(page, api)
  const reply = page.locator('.reply').last()
  await expect(reply.locator('ol > li > pre')).toHaveText('go build ./...\n')
  await expect(reply.locator('ol > li')).toHaveCount(2)
  await expect(reply.locator('table td code')).toHaveText('internal/serve')
  await expect(reply.locator('a')).toHaveAttribute('href', 'https://example.com/docs?a=1&b=2')
  await expect(reply).toContainText(
    'Tom & Jerry <img src=x onerror=alert(1)> <script>alert(2)</script> [x](javascript:alert(3))',
  )
  await expect(reply.locator('img, script, a[href^="javascript"]')).toHaveCount(0)
  expect(dialogs).toBe(0)

  const peer = page.locator('.prompt.from').last()
  await expect(peer.locator('.who')).toHaveText('from an agent')
  await expect(peer).toHaveClass(/folded/)
  await peer.getByRole('button', { name: 'Show all' }).click()
  await expect(peer).not.toHaveClass(/folded/)
})

test('shows a prompt it sent until its turns show it, and then only there', async ({ page }) => {
  await paired(page)
  await open(page, docs)
  await expect(page.getByText('Reading its turns…')).toHaveCount(0)
  const sent = page.locator('.prompt.sent')
  const shown = page.locator('.prompt:not(.sent)', { hasText: /^block$/ })
  const before = await shown.count()
  // The turns read after the send come late, as over a slow network.
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('turns'),
    async (route) => {
      await new Promise((r) => setTimeout(r, 1500))
      await route.continue()
    },
  )
  await prompt(page, 'block')
  try {
    await expect(sent.locator('.who')).toHaveText('sent')
    await expect(sent).toContainText('block')
    // Pi's turn waits on its dialog: only its turns showing the prompt take
    // it away.
    await expect(sent).toHaveCount(0)
    expect(session(docs)?.state).not.toBe('idle')
    await expect(shown).toHaveCount(before + 1)
  } finally {
    // The person answers the dialog in the terminal.
    tmux('send-keys', '-t', `main:${docs.name}`, 'yes', 'Enter')
  }
})

test('reads the turn that ended again until its reply is written', async ({ page }, info) => {
  await paired(page)
  await open(page, docs)
  await expect(page.locator('.timeline')).toBeVisible()
  // For half a second after the turn ends, its reply is not written yet, as
  // Claude Code may write it after its Stop hook, and no event follows.
  const text = `a short task on ${info.project.name}`
  let unwritten = 0
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('turns'),
    async (route) => {
      const res = await route.fetch()
      const doc = await res.json()
      const last = doc.turns.at(-1)
      if (last?.reply === `answered: ${text}` && (!unwritten || Date.now() - unwritten < 500)) {
        unwritten ||= Date.now()
        last.reply = ''
      }
      await route.fulfill({ response: res, json: doc })
    },
  )
  await prompt(page, text)
  await expect.poll(() => unwritten).toBeGreaterThan(0)
  await expect(page.locator('.reply').last()).toHaveText(`answered: ${text}`)
})

test('forgets the device without reading on after it', async ({ page }, info) => {
  await paired(page)
  await open(page, docs)
  await expect(page.locator('.timeline')).toBeVisible()
  // The turn ends with its reply unwritten, so the page means to read it again.
  const text = `a task to leave on ${info.project.name}`
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('turns'),
    async (route) => {
      const res = await route.fetch()
      const doc = await res.json()
      const last = doc.turns?.at(-1)
      if (last?.reply === `answered: ${text}`) last.reply = ''
      await route.fulfill({ response: res, json: doc })
    },
  )
  await prompt(page, text)
  await expect(page.locator('.turn').last().locator('.muted')).toHaveText('No reply', { timeout: 15_000 })
  await back(page)
  await computers(page)
  await page.getByRole('button', { name: 'Forget this computer' }).click()
  await expect(page.locator('.confirm')).toContainText(`tmag auth revoke`)
  await page.getByRole('button', { name: 'Forget', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible()
  await page.waitForTimeout(3500)
  await expect(page.locator('.notice')).toHaveCount(0)
})

test('allows a permission, and the title counts what waits', async ({ page }) => {
  await paired(page)
  await open(page, api)
  await prompt(page, 'block')
  const card = page.locator('.card')
  await expect(card.getByRole('heading')).toHaveText('Allow Bash?')
  await expect(card.locator('.asks pre').first()).toHaveText('date')
  await expect(page).toHaveTitle('(1) tmag')
  await card.getByRole('button', { name: 'Allow' }).click()
  await expect(card).toHaveCount(0)
  await expect.poll(() => said(api).at(-1)).toBe('hook:allow')
  await expect(page).toHaveTitle('tmag')
})

test('keeps what is typed in a card while the watch reports other sessions, and denies with it', async ({ page }) => {
  const [c] = await paired(page)
  await open(page, api)
  await prompt(page, 'block')
  const reason = page.getByRole('textbox', { name: 'Reason' })
  await reason.click()
  await page.keyboard.type('half a rea')
  // Another session starts and ends a turn while the person types.
  await page.evaluate(
    async ({ token, target }) => {
      await fetch('/v2/act', {
        method: 'POST',
        headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ verb: 'send', target, text: 'meanwhile' }),
      })
    },
    { token: c?.token ?? '', target: web.session },
  )
  await expect.poll(() => said(web).includes('meanwhile')).toBe(true)
  await idle(web)
  await page.keyboard.type('son')
  await expect(reason).toBeFocused()
  await expect(reason).toHaveValue('half a reason')
  await page.getByRole('button', { name: 'Deny' }).click()
  await expect(page.locator('.card')).toHaveCount(0)
  await expect.poll(() => said(api).at(-1)).toBe('hook:deny:half a reason')
})

test('answers a question', async ({ page }) => {
  await paired(page)
  await open(page, api)
  await prompt(page, 'ask')
  const card = page.locator('.card')
  await expect(card.locator('legend')).toHaveText('Pick a color')
  await card.getByRole('button', { name: 'Answer' }).click()
  await expect(card.getByRole('alert')).toHaveText('Answer each question.')
  await card.getByRole('button', { name: 'blue' }).click()
  await expect(card.getByRole('button', { name: 'blue' })).toHaveAttribute('aria-pressed', 'true')
  await card.getByRole('button', { name: 'Answer' }).click()
  await expect(card).toHaveCount(0)
  await expect.poll(() => said(api).at(-1)).toBe('hook:allow Pick a color=blue')
})

test('stops a turn, and shows the screen of its pane', async ({ page }) => {
  await paired(page)
  await open(page, api)
  await prompt(page, 'a long task')
  await expect(page.locator('.running')).toHaveText('working…')
  await page.getByRole('button', { name: 'Stop' }).click()
  await expect.poll(() => said(api).at(-1)).toBe('interrupted')

  await page.getByRole('button', { name: 'Screen' }).click()
  await expect(page.locator('pre.terminal')).toContainText('ready')
  await page.getByRole('button', { name: 'Turns' }).click()
  await expect(page.locator('.timeline')).toBeVisible()
})

test('keeps the screen current while it shows', async ({ page }, info) => {
  await paired(page)
  await open(page, docs)
  await page.getByRole('button', { name: 'Screen' }).click()
  await expect(page.locator('pre.terminal')).toContainText('ready')
  // The agent writes to its terminal, which no event reports.
  const tty = tmux('display-message', '-p', '-t', `main:${docs.name}`, '#{pane_tty}').trim()
  appendFileSync(tty, `\r\nstill thinking on ${info.project.name}\r\n`)
  await expect(page.locator('pre.terminal')).toContainText(`still thinking on ${info.project.name}`)
})

test('says in its own words why tmag refused a prompt, and keeps the prompt', async ({ page }) => {
  await paired(page)
  await open(page, web)
  await prompt(page, 'a long task')
  await expect.poll(() => session(web)?.state).toBe('working')
  // Codex in a terminal takes a prompt only while it is idle.
  await prompt(page, 'meanwhile')
  await expect(page.locator('.toast')).toHaveText(
    'Not sent: it cannot take a prompt while it works, or has not taken the last one yet. Send it again once it is idle.',
  )
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('meanwhile')
})

test('says when tmag serve is gone, and picks up where it was when it is back', async ({ page }) => {
  await paired(page)
  await open(page, api)
  await expect(page.locator('.turn').first()).toBeVisible()
  stopServe()
  await expect(page.getByRole('status').first()).toContainText('Trying again…')
  startServe()
  await expect(page.locator('.bar')).toHaveCount(0, { timeout: 20_000 })
  await prompt(page, 'after the restart')
  await expect(page.locator('.reply').last()).toHaveText('answered: after the restart', { timeout: 15_000 })
})

test('says when tmag on the computer is older than the page needs', async ({ page }) => {
  await paired(page)
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { version: '0.6.0', operations: undefined }),
  )
  await page.goto('/')
  await expect(page.locator('.row').first()).toBeVisible()
  await expect(page.locator('.bar')).toHaveText(
    'tmag 0.6.0 on the computer is older than this page needs. Update it to 0.7.0 or later.',
  )
})

test('asks to pair again once the credential is revoked', async ({ page }) => {
  const [c] = await paired(page)
  await page.goto('/')
  await expect(page.locator('.row').first()).toBeVisible()
  person(`tmag auth revoke ${c?.id}`)
  await expect(page.locator('.notice')).toHaveText(
    'The computer no longer accepts this device: its credential was revoked or expired. Pair it again.',
  )
})

test('tells how to let tmag serve accept an address it refuses', async ({ page }) => {
  await page.goto('http://127.0.0.1:4173/')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.getByRole('alert')).toHaveText(
    "tmag serve does not accept this page's address. On the computer, set -g @tmag-serve '--host 127.0.0.1:4173 --origin http://127.0.0.1:4173' and restart serve.",
  )
})

test('works with several computers as one inbox, each by a credential of its own', async ({ page }) => {
  await paired(page)
  await page.goto('/')
  await expect(page.locator('.row .name', { hasText: 'api' })).toBeVisible()
  await expect(row(page, ops)).toHaveCount(0)

  await computers(page)
  await page.getByRole('link', { name: 'Add a computer' }).click()
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('two')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  // Paired, it goes back to the computers, and they to the inbox.
  await expect(page.locator('.computer')).toHaveCount(2)
  await back(page)
  await expect(row(page, ops).locator('.meta')).toContainText('· two')
  await expect(row(page, api).locator('.meta')).toContainText('· localhost')

  await tap(page, ops)
  await expect(page).toHaveURL(new RegExp(`#/two/s/${ops.session}$`))
  await prompt(page, 'from the other computer')
  await expect(page.locator('.reply').last()).toHaveText('answered: from the other computer', { timeout: 15_000 })
  expect(said(ops)).toContain('from the other computer')

  // The first computer at a second address is the same computer.
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('again')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.getByRole('alert')).toContainText(
    'That is localhost, which the page already works with. On it, revoke the credential just made: tmag auth revoke ',
  )

  await page.goto('/')
  const two = (await computers(page)).filter({ hasText: '/two/' })
  await two.getByRole('button', { name: 'Forget this computer' }).click()
  await two.getByRole('button', { name: 'Forget', exact: true }).click()
  await expect(page.locator('.computer')).toHaveCount(1)
  await back(page)
  await expect(row(page, ops)).toHaveCount(0)
  await expect(row(page, api)).toBeVisible()
})

test('marks a turn that ended unseen until the session is opened', async ({ page }) => {
  await paired(page)
  await page.goto('/')
  const apiRow = row(page, api)
  await expect(apiRow).toBeVisible()
  await expect(apiRow).not.toHaveClass(/done/)
  person(`tmag send ${api.session} 'done while away'`)
  await expect(apiRow).toHaveClass(/done/, { timeout: 15_000 })
  await expect(apiRow.locator('.meta')).toContainText('Claude Code · done')
  await expect(page).toHaveTitle('(1) tmag')
  await apiRow.click()
  await expect(page.locator('.reply').last()).toHaveText('answered: done while away')
  await back(page)
  await expect(apiRow).not.toHaveClass(/done/)
  await expect(page).toHaveTitle('tmag')
  await page.reload()
  await expect(apiRow).toBeVisible()
  await expect(apiRow).not.toHaveClass(/done/)
})

test('shows whom an agent works for and whom it waits on', async ({ page }) => {
  test.skip(!serves('start'), 'tmag records asks where it starts agents, after 0.7.0')
  await paired(page)
  await open(page, web)
  tmux('send-keys', '-t', `main:${api.name}`, `tmag send ${web.session} --reply review the parser`, 'Enter')
  const ties = page.locator('header .ties')
  await expect(ties.getByRole('link')).toHaveText('owes a reply to api', { timeout: 15_000 })
  await ties.getByRole('link').click()
  await expect(page.locator('header .name')).toHaveText('api')
  await expect(ties.getByRole('link')).toHaveText('asked web')
  // Back goes to the session it came from, and from there to the inbox.
  await back(page)
  await expect(page.locator('header .name')).toHaveText('web')
  await back(page)
  await expect(row(page, web).locator('.meta')).toContainText('owes a reply to api')
  await idle(web)
  tmux('send-keys', '-t', `main:${web.name}`, `tmag reply ${api.session} done`, 'Enter')
  await expect(row(page, web).locator('.meta')).not.toContainText('owes', {
    timeout: 15_000,
  })
})

test('opens the request a link names, and says when it is no longer open', async ({ page }) => {
  await paired(page)
  await open(page, api)
  await prompt(page, 'block')
  await expect(page.locator('.card')).toHaveCount(1)
  const request = session(api)?.requests[0]?.id ?? ''
  // Over what the directory changed, the request takes one line; the link
  // that names it shows it with the turns.
  if (serves('changes')) {
    await page.getByRole('button', { name: 'Changes' }).click()
    await expect(page.getByRole('button', { name: /Allow Bash\?/ })).toHaveAttribute('aria-expanded', 'false')
  }
  await page.goto(`/#/s/${api.session}/r/${request}`)
  await expect(page.getByRole('button', { name: 'Turns' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.locator('.card.focus').getByRole('heading')).toHaveText('Allow Bash?')
  await page.goto(`/#/s/${api.session}/r/answered-before`)
  await expect(page.locator('.card.muted')).toHaveText('The request this link names is no longer open.')
  await page.locator('.card:not(.muted)').getByRole('button', { name: 'Allow' }).click()
  await expect.poll(() => said(api).at(-1)).toBe('hook:allow')
})

test('keeps what was seen of the other sessions while the computer is slow to say which it is', async ({ page }) => {
  await paired(page)
  await page.goto('/')
  await expect(row(page, api)).toBeVisible()
  person(`tmag send ${api.session} 'first unseen'`)
  person(`tmag send ${web.session} 'second unseen'`)
  await expect(row(page, api)).toHaveClass(/done/, { timeout: 15_000 })
  await expect(row(page, web)).toHaveClass(/done/, { timeout: 15_000 })
  await page.route(
    (url) => url.pathname === '/v2/info',
    async (route) => {
      await new Promise((r) => setTimeout(r, 1500))
      await route.continue()
    },
  )
  await open(page, api)
  await expect(page.locator('.reply').last()).toHaveText('answered: first unseen')
  await back(page)
  await expect(row(page, api)).not.toHaveClass(/done/)
  await expect(row(page, web)).toHaveClass(/done/)
})

test('sees a turn that ended while the page was hidden only once it shows', async ({ page }) => {
  const hide = (hidden: boolean) =>
    page.evaluate((hidden) => {
      Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
      document.dispatchEvent(new Event('visibilitychange'))
    }, hidden)
  await paired(page)
  await open(page, web)
  await expect(page.locator('.timeline')).toBeVisible()
  await hide(true)
  person(`tmag send ${web.session} 'while away'`)
  await expect(page.locator('.reply').last()).toHaveText('answered: while away', { timeout: 15_000 })
  await back(page)
  await expect(row(page, web)).toHaveClass(/done/)

  await tap(page, web)
  await person(`tmag send ${web.session} 'while away again'`)
  await expect(page.locator('.reply').last()).toHaveText('answered: while away again', { timeout: 15_000 })
  await hide(false)
  await back(page)
  await expect(row(page, web)).not.toHaveClass(/done/)
})

test('says why it forgot a computer while it works with another', async ({ page }) => {
  const [, two] = await paired(page, '', 'two')
  await page.goto('/')
  await expect(row(page, ops)).toBeVisible()
  person(`tmag auth revoke ${two?.id}`, 'two')
  await expect(page.getByRole('alert')).toContainText(
    'The computer two no longer accepts this device: its credential was revoked or expired. Pair it again.',
    { timeout: 15_000 },
  )
  await expect(row(page, ops)).toHaveCount(0)
  await expect(row(page, api)).toBeVisible()
  await page.getByRole('link', { name: 'Pair it again' }).click()
  await expect(page.getByRole('heading', { name: 'Add a computer' })).toBeVisible()
})

test('keeps a credential the computer issued when it does not say at once which it is', async ({ page }) => {
  await paired(page)
  let failed = false
  await page.route(
    (url) => url.pathname === '/two/v2/info',
    (route) => {
      if (failed) return route.continue()
      failed = true
      return route.fulfill({ status: 503, json: { schema: 'tmag/http/v2', error: { code: 'busy', message: 'later' } } })
    },
  )
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('two')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(row(page, ops)).toBeVisible({ timeout: 15_000 })
  expect(failed).toBe(true)
  await expect(await computers(page)).toHaveCount(2)
})

test('offers only what the computer serves', async ({ page }) => {
  await paired(page)
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { version: '0.7.0', operations: ['list', 'inspect', 'watch', 'read', 'request'] }),
  )
  await open(page, api)
  await expect(page.locator('.composer')).toHaveText('tmag has no way to prompt this session.')
})

test('goes on to what a link names once the computer it names is added', async ({ page }) => {
  await paired(page)
  await page.goto(`/#/two/s/${ops.session}`)
  await page.getByRole('button', { name: 'Add it' }).click()
  await expect(page.getByRole('heading', { name: 'Add a computer' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Name' })).toHaveValue('two')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page).toHaveURL(new RegExp(`#/two/s/${ops.session}$`))
  await expect(page.locator('header .name')).toHaveText('ops')
})

test('keeps the pairing a page made before it worked with several computers', async ({ page }) => {
  const c = await credential()
  await page.addInitScript((c) => {
    if (!localStorage.getItem('tmag.computers')) localStorage.setItem('tmag.credential', JSON.stringify(c))
  }, c)
  await page.goto('/')
  await expect(row(page, api)).toBeVisible()
  expect(await page.evaluate(() => localStorage.getItem('tmag.credential'))).toBeNull()
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('tmag.computers') ?? '[]').length)).toBe(1)
})

test('works with a computer once, though it said which it is only after another address of it was added', async ({
  page,
}) => {
  await paired(page)
  let refused = 0
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => (refused++ < 1 ? route.fulfill({ status: 503, json: {} }) : route.continue()),
  )
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('again')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.getByRole('alert')).toContainText(
    'That is localhost, which the page already works with. On it, revoke the credential just made: tmag auth revoke ',
    { timeout: 20_000 },
  )
  await page.goto('/')
  await expect(row(page, api)).toHaveCount(1)
  await expect(await computers(page)).toHaveCount(1)
})

test('keeps a credential just issued while the computer has not said which it is, across a reload', async ({
  page,
}) => {
  await paired(page)
  let hold = true
  await page.route(
    (url) => url.pathname === '/two/v2/info',
    async (route) => {
      while (hold) await new Promise((r) => setTimeout(r, 100))
      await route.continue()
    },
  )
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('two')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect
    .poll(() => page.evaluate(() => JSON.parse(localStorage.getItem('tmag.computers') ?? '[]').length))
    .toBe(2)
  await page.goto('/')
  await expect(row(page, ops)).toHaveCount(0)
  hold = false
  await expect(row(page, ops)).toBeVisible({ timeout: 20_000 })
})

test('works once with a computer it kept twice', async ({ page }) => {
  const [a, b] = [await credential(), await credential()]
  const info = await page.request.get('/v2/info', { headers: { Authorization: `Bearer ${a?.token}` } })
  const { instance } = await info.json()
  await page.addInitScript(
    (s) => {
      if (!localStorage.getItem('tmag.computers')) localStorage.setItem('tmag.computers', JSON.stringify(s))
    },
    [
      { name: '', credential: a, instance },
      { name: 'again', credential: b, instance },
    ],
  )
  await page.goto('/')
  await expect(page.getByRole('alert')).toContainText('That is localhost, which the page already works with.')
  await expect(row(page, api)).toHaveCount(1)
  await expect(await computers(page)).toHaveCount(1)
})

test('reads nothing of a session its computer does not serve', async ({ page }) => {
  await paired(page)
  const reads: string[] = []
  page.on('request', (r) => new URL(r.url()).pathname === '/v2/read' && reads.push(r.url()))
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { version: '0.7.0', operations: ['list', 'watch'] }),
  )
  await open(page, api)
  await expect(page.locator('main .empty')).toHaveText("tmag on this computer does not read a session's turns.")
  expect(reads).toEqual([])
})

test('forgets where a link went once the person leaves adding its computer', async ({ page }) => {
  await paired(page)
  await page.goto(`/#/two/s/${ops.session}`)
  await page.getByRole('button', { name: 'Add it' }).click()
  await expect(page.getByRole('textbox', { name: 'Name' })).toHaveValue('two')
  // Back goes to the link it came from, and on to the sessions.
  await back(page)
  await expect(page.getByRole('button', { name: 'Add it' })).toBeVisible()
  await back(page)
  await computers(page)
  await page.getByRole('link', { name: 'Add a computer' }).click()
  await page.getByRole('checkbox').check()
  await expect(page.getByRole('textbox', { name: 'Name' })).toHaveValue('')
})

test('watches a computer once, though the page wakes while it says which it is', async ({ page }) => {
  await paired(page)
  const watches: string[] = []
  page.on('request', (r) => new URL(r.url()).pathname === '/v2/watch' && watches.push(r.url()))
  let hold = true
  await page.route(
    (url) => url.pathname === '/v2/info',
    async (route) => {
      while (hold) await new Promise((r) => setTimeout(r, 100))
      await route.continue()
    },
  )
  await page.goto('/')
  await page.evaluate(() => dispatchEvent(new Event('online')))
  hold = false
  await expect(row(page, api)).toBeVisible()
  await page.waitForTimeout(1000)
  expect(watches).toHaveLength(1)
})

test('sends to a session of a computer that serves sends but not reads', async ({ page }) => {
  await paired(page)
  const reads: string[] = []
  page.on('request', (r) => new URL(r.url()).pathname === '/v2/read' && reads.push(r.url()))
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { version: '0.7.0', operations: ['list', 'watch', 'send'] }),
  )
  await open(page, api)
  await prompt(page, 'sent without reading')
  await expect.poll(() => said(api)).toContain('sent without reading')
  expect(reads).toEqual([])
})

test('leaves the person where they went while a pairing they left ends', async ({ page }) => {
  await paired(page)
  let hold = true
  await page.route(
    (url) => url.pathname === '/two/v2/info',
    async (route) => {
      while (hold) await new Promise((r) => setTimeout(r, 100))
      await route.continue()
    },
  )
  await page.goto(`/#/two/s/${ops.session}`)
  await page.getByRole('button', { name: 'Add it' }).click()
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect
    .poll(() => page.evaluate(() => JSON.parse(localStorage.getItem('tmag.computers') ?? '[]').length))
    .toBe(2)
  await back(page)
  await expect(page).toHaveURL(new RegExp(`#/two/s/${ops.session}$`))
  await back(page)
  await expect(page.getByRole('heading', { name: 'Inbox' })).toBeVisible()
  await tap(page, api)
  // The pairing ends once the computer it added is watched.
  const watched = page.waitForRequest((r) => new URL(r.url()).pathname === '/two/v2/watch')
  hold = false
  await watched
  await expect(page).toHaveURL(new RegExp(`#/s/${api.session}$`))
})

test('starts an agent from the page, and opens it', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  const sid = '0e000000-0000-7000-8000-00000000000e'
  const started = startable({ FAKE_SESSION: sid, FAKE_HOOKS: '1', FAKE_TURN_MS: '500' })
  try {
    await paired(page)
    await page.goto('/')
    await page.getByRole('link', { name: 'New agent', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'New agent' })).toBeVisible()
    await picked(page).click()
    await page.getByRole('textbox', { name: 'Directory' }).fill(session(api)?.cwd ?? '')
    await page.getByRole('textbox', { name: 'Prompt' }).fill('from the start sheet')
    await page.getByRole('button', { name: 'Start', exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`#/s/${sid}$`), { timeout: 30_000 })
    await expect(page.locator('.reply').last()).toHaveText('answered: from the start sheet', { timeout: 15_000 })
  } finally {
    started.stop()
  }
})

// stalled answers a start with the 504 of an agent whose pane shows no
// session in time, as tmag gives it, once go is called.
function stalled(page: Page, pane: { server: string; id: string }) {
  let go = () => {}
  const held = new Promise<void>((r) => {
    go = r
  })
  void page.route(
    (url) => url.pathname === '/v2/act',
    async (route) => {
      if (route.request().postDataJSON().verb !== 'start') return route.continue()
      await held
      await route.fulfill({
        status: 504,
        json: {
          schema: 'tmag/v2',
          error: {
            code: 'timeout',
            message: `the agent started in ${pane.id}, which shows no session tmag can prove yet`,
          },
          pane,
        },
      })
    },
  )
  return go
}

test('opens the pane of an agent that shows no session yet, with keys to answer it and its prompt kept', async ({
  page,
}) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  const pane = paneOf(docs)
  stalled(page, pane)()
  await page.goto('/#/start')
  // Opened before the page knows its sessions, it chooses where they run once it does.
  await expect(picked(page)).toContainText('/')
  await picked(page).click()
  await page.getByRole('textbox', { name: 'Directory' }).fill(session(docs)?.cwd ?? '')
  await page.getByRole('textbox', { name: 'Prompt' }).fill('held for its session')
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await expect(page.getByRole('heading', { name: `Pane ${pane.id}` })).toBeVisible()
  await expect(page.locator('.terminal')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Escape' })).toBeVisible()
  // Its pane shows a session, as the agent's would once its dialog is answered.
  await page.getByRole('link', { name: 'Open its session' }).click()
  await expect(page.locator('header .name')).toHaveText('docs')
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('held for its session')
})

test('leaves the person where they went while a start they left ends', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  const pane = paneOf(docs)
  const go = stalled(page, pane)
  await startIn(page, docs)
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await back(page)
  await tap(page, api)
  go()
  await page.getByRole('link', { name: 'Open its pane' }).click()
  await expect(page.getByRole('heading', { name: `Pane ${pane.id}` })).toBeVisible()
  await page.goBack()
  await expect(page).toHaveURL(new RegExp(`#/s/${api.session}$`))
})

test('leaves the person on the start sheet of another computer while a start they left there ends', async ({
  page,
}) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page, '', 'two')
  const pane = paneOf(docs)
  const go = stalled(page, pane)
  await startIn(page, docs)
  await expect(page.getByRole('radio', { name: 'localhost' })).toBeChecked()
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await page.goto('/#/two/start')
  await expect(page.getByRole('radio', { name: 'two' })).toBeChecked()
  go()
  await expect(page.getByRole('link', { name: 'Open its pane' })).toBeVisible()
  await expect(page).toHaveURL(/#\/two\/start$/)
})

test('starts on the computer its link names, once that computer says it starts agents', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page, '', 'two')
  await page.goto('/#/two/start')
  await expect(page.getByRole('radio', { name: 'two' })).toBeChecked()
})

// startFailed answers a start with an agent that started and a prompt that
// then failed with error.
function startFailed(page: Page, status: number, error: { code: string; message: string }) {
  return page.route(
    (url) => url.pathname === '/v2/act',
    (route) =>
      route.fulfill({ status, json: { schema: 'tmag/v2', error, started: { id: api.session, agent: 'claude' } } }),
  )
}

test('says when a started agent did not get its prompt, and keeps the prompt for it', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  await startFailed(page, 409, { code: 'refused:busy', message: 'it works' })
  await startIn(page, api)
  await page.getByRole('textbox', { name: 'Prompt' }).fill('never given')
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText(
    'It started, but tmag did not give it the prompt, which waits in its message: it cannot take a prompt while it works',
  )
  await page.getByRole('link', { name: 'Open it' }).click()
  await expect(page.locator('header .name')).toHaveText('api')
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('never given')
})

test('says when it is unknown whether a started agent got its prompt, and keeps no copy in its message', async ({
  page,
}) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  await startFailed(page, 500, { code: 'error', message: 'delivered but not logged; do not repeat it' })
  await startIn(page, api)
  await page.getByRole('textbox', { name: 'Prompt' }).fill('maybe given')
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('It started, and whether it got the prompt is unknown')
  await page.getByRole('link', { name: 'Open it' }).click()
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('')
  // The sheet still holds it, for the person to see what it was.
  await back(page)
  await expect(page.getByRole('textbox', { name: 'Prompt' })).toHaveValue('maybe given')
})

test('shows no screen of a pane tmag no longer finds', async ({ page }) => {
  await paired(page)
  let gone = false
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('lines'),
    (route) =>
      gone
        ? route.fulfill({
            status: 404,
            json: { schema: 'tmag/v2', error: { code: 'not_found', message: 'not found in pane %1' } },
          })
        : route.continue(),
  )
  await page.goto(`/#/p/${encodeURIComponent(paneOf(api).id)}`)
  await expect(page.locator('.terminal')).toContainText('ready')
  gone = true
  await expect(page.locator('.terminal')).toHaveText('not found in pane %1')
  if (serves('keys')) await expect(page.getByRole('button', { name: 'Escape' })).toBeDisabled()
})

test('reads no pane on a computer that serves no reads', async ({ page }) => {
  await paired(page)
  const reads: string[] = []
  page.on('request', (r) => new URL(r.url()).pathname === '/v2/read' && reads.push(r.url()))
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { version: '0.7.0', operations: ['list', 'watch', 'keys'] }),
  )
  await page.goto(`/#/p/${encodeURIComponent(paneOf(api).id)}`)
  await expect(page.locator('.empty')).toHaveText('tmag on this computer does not read a pane.')
  await expect(page.getByRole('button', { name: 'Escape' })).toHaveCount(0)
  expect(reads).toEqual([])
})

test('types into what a session shows, on the screen the person reads', async ({ page }) => {
  test.skip(!serves('keys'), 'tmag types keys after 0.7.0')
  await paired(page)
  await open(page, api)
  await page.getByRole('button', { name: 'Screen' }).click()
  await expect(page.locator('.terminal')).toContainText('ready')
  // The line in the field goes before whichever key is pressed; what the
  // person writes while it is typed waits for the next key, which waits for
  // the screen to be read again.
  await page.getByRole('textbox', { name: 'Type' }).fill('typed on')
  await page.getByRole('button', { name: 'Space', exact: true }).click()
  await page.getByRole('textbox', { name: 'Type' }).fill('the screen')
  await page.getByRole('button', { name: 'Enter' }).click()
  await expect.poll(() => said(api)).toContain('typed on the screen')
  await expect(page.getByRole('textbox', { name: 'Type' })).toHaveValue('')
})

test('waits to type again until the screen is read again', async ({ page }) => {
  test.skip(!serves('keys'), 'tmag types keys after 0.7.0')
  await paired(page)
  let failing = false
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('lines'),
    (route) =>
      failing
        ? route.fulfill({ status: 503, json: { schema: 'tmag/v2', error: { code: 'backend', message: 'not now' } } })
        : route.continue(),
  )
  await page.route(
    (url) => url.pathname === '/v2/act',
    async (route) => {
      const res = await route.fetch()
      failing = true
      await route.fulfill({ response: res })
    },
  )
  await open(page, api)
  await page.getByRole('button', { name: 'Screen' }).click()
  await page.getByRole('button', { name: 'Escape' }).click()
  await expect(page.locator('.stale')).toContainText('Not read again: not now')
  await expect(page.getByRole('button', { name: 'Escape' })).toBeDisabled()
  // The two-second reads fail too: the bar waits for one that does not.
  await page.waitForTimeout(2500)
  await expect(page.getByRole('button', { name: 'Escape' })).toBeDisabled()
  failing = false
  await expect(page.getByRole('button', { name: 'Escape' })).toBeEnabled()
  await expect(page.locator('.stale')).toHaveCount(0)
})

test('types nothing on a screen that changed since it was read', async ({ page }) => {
  test.skip(!serves('keys'), 'tmag types keys after 0.7.0')
  await paired(page)
  await page.route(
    (url) => url.pathname === '/v2/read' && url.searchParams.has('lines'),
    async (route) => {
      const res = await route.fetch()
      const doc = await res.json()
      doc.screen = doc.screen.replace(/:.*$/, `:${'0'.repeat(64)}`)
      await route.fulfill({ response: res, json: doc })
    },
  )
  await open(page, api)
  await page.getByRole('button', { name: 'Screen' }).click()
  const before = said(api).length
  await page.getByRole('button', { name: 'Escape' }).click()
  await expect(page.getByRole('status').last()).toHaveText(
    'Not typed: the screen changed before it was typed. Look at it again.',
  )
  expect(said(api).length).toBe(before)
})

// repository makes the directory an agent runs in a git repository in which
// parse.go gained a function, for as long as run runs.
async function repository(a: Agent, run: (dir: string) => Promise<void>) {
  const dir = session(a)?.cwd ?? ''
  const git = (...args: string[]) =>
    execFileSync('git', ['-C', dir, '-c', 'user.name=t', '-c', 'user.email=t@example.com', ...args])
  try {
    git('init', '-q')
    writeFileSync(join(dir, '.gitignore'), 'out\n*.jsonl\n')
    writeFileSync(join(dir, 'parse.go'), 'package parse\n')
    git('add', '.')
    git('commit', '-q', '-m', 'init')
    writeFileSync(join(dir, 'parse.go'), 'package parse\n\nfunc Parse() {}\n')
    await run(dir)
  } finally {
    rmSync(join(dir, '.git'), { recursive: true, force: true })
    rmSync(join(dir, '.gitignore'), { force: true })
    rmSync(join(dir, 'parse.go'), { force: true })
  }
}

test("shows what an agent's directory changed, and puts what the person says of a line in its message", async ({
  page,
}) => {
  test.skip(!serves('changes'), 'tmag shows changes after 0.7.0')
  await repository(api, async (dir) => {
    await paired(page)
    await open(page, api)
    await page.getByRole('button', { name: 'Changes' }).click()
    const file = page.getByRole('button', { name: /parse\.go/ })
    await expect(file).toContainText('+2 −0')
    await file.click()
    await page.locator('.patch .line.added', { hasText: 'func Parse() {}' }).click()
    await page.getByRole('textbox', { name: 'Comment' }).fill('name it Read')
    // The agent changes the file while the comment is written: the comment
    // stays with its line, and is sent as the file numbers that line now.
    writeFileSync(join(dir, 'parse.go'), 'package parse\n\n// Parse parses.\nfunc Parse() {}\n')
    person(`tmag send ${api.session} 'look again'`)
    await expect(page.locator('.patch .line.added', { hasText: '// Parse parses.' })).toBeVisible()
    await expect(page.getByRole('textbox', { name: 'Comment' })).toHaveValue('name it Read')
    await page.getByRole('button', { name: 'Add' }).click()
    await expect(page.locator('.patch .comment').locator('..').locator('.code')).toHaveText('func Parse() {}')
    await page.getByRole('button', { name: 'Put in the message' }).click()
    await expect(page.getByRole('button', { name: 'Turns' })).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue(
      'parse.go:4: name it Read\n> func Parse() {}',
    )
    await idle(api)
    await page.getByRole('button', { name: 'Send' }).click()
    await expect.poll(() => said(api).join('\n')).toContain('parse.go:4: name it Read\n> func Parse() {}')
    // Its turn ends, so that the next test finds the agent idle.
    await expect(page.locator('.reply').last()).toHaveText('answered: > func Parse() {}')
  })
})

test('keeps the comment being written across a reload, and while its file goes back to HEAD', async ({ page }) => {
  test.skip(!serves('changes'), 'tmag shows changes after 0.7.0')
  await repository(api, async (dir) => {
    await paired(page)
    await open(page, api)
    await page.getByRole('button', { name: 'Changes' }).click()
    await page.getByRole('button', { name: /parse\.go/ }).click()
    await page.locator('.patch .line.added', { hasText: 'func Parse() {}' }).click()
    await page.getByRole('textbox', { name: 'Comment' }).fill('kept while it goes')
    // One comment at a time: no other line begins one meanwhile.
    await expect(page.locator('.patch button.line')).toHaveCount(0)
    await page.reload()
    await page.getByRole('button', { name: 'Changes' }).click()
    await expect(page.getByRole('textbox', { name: 'Comment' })).toHaveValue('kept while it goes')
    writeFileSync(join(dir, 'parse.go'), 'package parse\n')
    person(`tmag send ${api.session} 'look again'`)
    await expect(page.locator('.changes .empty')).toHaveText('Nothing changed against HEAD.')
    await expect(page.locator('.changes > .moved')).toContainText('Writing on parse.go:3')
    await expect(page.getByRole('textbox', { name: 'Comment' })).toHaveValue('kept while it goes')
    await page.getByRole('button', { name: 'Add' }).click()
    await expect(page.locator('.put')).toContainText('1 comment')
    await idle(api)
  })
})

test('says when what a directory changed could not be read again, and takes no comment on it', async ({ page }) => {
  test.skip(!serves('changes'), 'tmag shows changes after 0.7.0')
  await repository(api, async () => {
    await paired(page)
    let failing = false
    await page.route(
      (url) => url.pathname === '/v2/changes',
      (route) =>
        failing
          ? route.fulfill({ status: 503, json: { schema: 'tmag/v2', error: { code: 'backend', message: 'not now' } } })
          : route.continue(),
    )
    await open(page, api)
    await page.getByRole('button', { name: 'Changes' }).click()
    await page.getByRole('button', { name: /parse\.go/ }).click()
    await expect(page.locator('.patch button.line.added')).toHaveCount(2)
    failing = true
    await page.getByRole('button', { name: 'Turns' }).click()
    await page.getByRole('button', { name: 'Changes' }).click()
    await expect(page.locator('.changes > .stale')).toContainText('Not read again: not now')
    await page.getByRole('button', { name: /parse\.go/ }).click()
    await expect(page.locator('.patch .stale')).toContainText('Not read again: not now')
    await expect(page.locator('.patch button.line')).toHaveCount(0)
    failing = false
    await page.locator('.patch .stale').getByRole('button', { name: 'Read again' }).click()
    await expect(page.locator('.patch button.line.added')).toHaveCount(2)
  })
})

test('goes on to what waits on the person, and answers a permission from the inbox', async ({ page }) => {
  await paired(page)
  await page.goto('/')
  await expect(page.getByText('Nothing waits on you.')).toBeVisible()
  person(`tmag send ${web.session} 'ended unseen'`)
  await expect(row(page, web)).toHaveClass(/done/, { timeout: 15_000 })
  await expect(page.getByRole('region', { name: 'Needs you' })).toContainText('web')
  await expect(row(page, web).locator('.said')).toHaveText('answered: ended unseen')
  await tap(page, api)
  await prompt(page, 'block')
  await expect(page.locator('.card')).toBeVisible()
  // The session that waits next opens in place of this one: back still goes to the inbox.
  await page.getByRole('link', { name: 'Next that waits on you, of 1' }).click()
  await expect(page.locator('header .name')).toHaveText('web')
  await expect(page.locator('.reply').last()).toHaveText('answered: ended unseen')
  await back(page)
  await expect(page.getByRole('heading', { name: 'Inbox' })).toBeVisible()
  await expect(row(page, web)).not.toHaveClass(/done/)
  // A permission shows what it would do, and takes its answer there.
  const asks = row(page, api)
  await expect(asks.locator('.ask')).toHaveText('Allow Bash?')
  await expect(asks.locator('.asks code')).toHaveText('date')
  await expect(page).toHaveTitle('(1) tmag')
  await asks.getByRole('button', { name: 'Allow' }).click()
  await expect.poll(() => said(api).at(-1)).toBe('hook:allow')
  await expect(asks.locator('.asks')).toHaveCount(0)
  await expect(page).toHaveTitle(/^(\(1\) )?tmag$/)
})

test('shows sessions by project, and starts another agent in one', async ({ page }) => {
  await paired(page, '', 'two')
  await page.goto('/#/projects')
  const project = page.getByRole('region', { name: 'api' })
  await expect(project.locator('li.row')).toHaveCount(1)
  await expect(project.locator('.row .name')).toHaveText('Claude Code')
  await expect(page.getByRole('region', { name: 'ops' })).toContainText('two')
  if (!serves('start')) return
  await project.getByRole('link', { name: 'New agent in api' }).click()
  await expect(page.getByRole('heading', { name: 'New agent' })).toBeVisible()
  await expect(picked(page)).toContainText(session(api)?.cwd ?? '')
  await expect(page.getByRole('radio', { name: 'Claude Code' })).toBeChecked()
  await back(page)
  await expect(page.getByRole('heading', { name: 'Projects' })).toBeVisible()
})

test('starts an agent again from the tile of a place where none runs now', async ({ page }) => {
  test.skip(!serves('dirs'), 'tmag lists directories after 0.8.0')
  // A place outside the agents' directories, which other tests list.
  const lab = join(process.env.TMAG_E2E_DIR ?? '', 'lab')
  mkdirSync(lab, { recursive: true })
  const sid = '0f000000-0000-7000-8000-00000000000f'
  const started = startable({ FAKE_SESSION: sid, FAKE_HOOKS: '1' })
  try {
    await paired(page)
    await page.goto(`/#/start/${encodeURIComponent(lab)}`)
    await page.getByRole('button', { name: 'Start', exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`#/s/${sid}$`), { timeout: 30_000 })
  } finally {
    started.stop()
  }
  await page.goto('/#/projects')
  const tile = page.getByRole('region', { name: 'Recent' }).getByRole('link', { name: /^lab: new agent in .*\/lab$/ })
  await expect(tile).toHaveText('lab', { timeout: 15_000 })
  await tile.click()
  await expect(page.getByRole('heading', { name: 'New agent' })).toBeVisible()
  await expect(picked(page)).toContainText(lab)
})

test('chooses where to start from where agents ran and the directories a path completes in', async ({ page }) => {
  test.skip(!serves('dirs'), 'tmag lists directories after 0.8.0')
  await paired(page)
  const cwd = session(api)?.cwd ?? ''
  const work = cwd.slice(0, cwd.lastIndexOf('/'))
  await page.goto('/#/start')
  await picked(page).click()
  // What it offers first is where agents ran lately.
  await expect(page.locator('.recent')).toContainText(cwd)
  const box = page.getByRole('textbox', { name: 'Directory' })
  await box.fill(`${work}/d`)
  const listed = page.locator('.browse .dir')
  await expect(listed).toHaveText(['docs'])
  await box.fill(`${work}/`)
  await expect(listed).toHaveText(['api', 'docs', 'ops', 'web'])
  await page.locator('.browse').getByRole('button', { name: 'api', exact: true }).click()
  await expect(box).toHaveValue(`${cwd}/`)
  await page.getByRole('button', { name: 'Up one level' }).click()
  await expect(box).toHaveValue(`${work}/`)
  await box.fill(cwd)
  await page.getByRole('button', { name: 'Use this directory' }).click()
  await expect(picked(page)).toContainText('api')
  // A directory tmag lists only part of may hold what it does not list.
  await page.route(
    (url) => url.pathname === '/v2/dirs' && url.searchParams.get('dir') === '/big',
    (route) =>
      route.fulfill({ json: { schema: 'tmag/v2', dir: { path: '/big', parent: '/', dirs: [], truncated: true } } }),
  )
  await picked(page).click()
  await box.fill('/big/x')
  await expect(page.locator('.browse .none')).toContainText('it lists only part of a directory this large')
  // A path that names no directory starts nothing.
  await box.fill(`${work}/nowhere`)
  await expect(page.locator('.browse .none')).toHaveText('No directory here starts so.')
  await expect(page.locator('.picker')).toContainText('No directory has this path.')
  await expect(page.getByRole('button', { name: 'Use this directory' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText(`${work}/nowhere`)
  await expect(page.getByRole('alert')).toContainText('is no')
})

test('answers in the list only a command it shows in full, and opens anything else to answer there', async ({
  page,
}) => {
  await paired(page)
  // What the request asks besides a command the row does not show: the
  // change to a file, a time limit, a command's arguments one by one.
  let input: Record<string, unknown> = { file_path: '/src/parse.go', old_string: 'a', new_string: 'b' }
  await page.route(
    (url) => url.pathname === '/v2/request',
    async (route) => {
      const res = await route.fetch()
      const doc = await res.json()
      doc.input = input
      await route.fulfill({ response: res, json: doc })
    },
  )
  await page.goto('/')
  person(`tmag send ${api.session} block`)
  const asks = row(page, api)
  await expect(asks.locator('.asks code')).toHaveText('/src/parse.go')
  await expect(asks.getByRole('button', { name: 'Allow' })).toHaveCount(0)
  for (const other of [{ command: 'date', timeout: 600000 }, { command: ['date'] }]) {
    input = other
    await page.reload()
    await expect(asks.getByRole('link', { name: 'Review' })).toBeVisible()
    await expect(asks.getByRole('button', { name: 'Allow' })).toHaveCount(0)
  }
  await asks.getByRole('link', { name: 'Review' }).click()
  await expect(page).toHaveURL(new RegExp(`#/s/${api.session}/r/`))
  await page.locator('.card.focus').getByRole('button', { name: 'Allow' }).click()
  await expect.poll(() => said(api).at(-1)).toBe('hook:allow')
})

test('starts nothing on another computer than the one a link names', async ({ page }) => {
  await paired(page)
  await page.goto(`/#/lab/start/${encodeURIComponent(session(api)?.cwd ?? '')}`)
  await expect(page.locator('main .empty')).toContainText('This page does not work with a computer named lab.')
  await expect(page.getByRole('button', { name: 'Start', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add it' })).toBeVisible()
})

test('starts in the very directory its link names', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  // A sibling whose name ends in a space is another directory.
  const dir = `${session(api)?.cwd ?? ''} `
  mkdirSync(dir)
  try {
    let asked = ''
    await page.route(
      (url) => url.pathname === '/v2/act',
      (route) => {
        asked = route.request().postDataJSON().dir
        return route.fulfill({
          status: 409,
          json: { schema: 'tmag/v2', error: { code: 'refused:busy', message: 'not now' } },
        })
      },
    )
    await page.goto(`/#/start/${encodeURIComponent(dir)}`)
    await page.getByRole('textbox', { name: 'Prompt' }).fill('in the one with a space')
    await page.getByRole('button', { name: 'Start', exact: true }).click()
    await expect(page.getByRole('alert')).toBeVisible()
    expect(asked).toBe(dir)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('keeps the directory it showed once the person acts on it, though the places come later', async ({ page }) => {
  test.skip(!serves('dirs'), 'tmag lists directories after 0.8.0')
  await paired(page)
  // The person writes a prompt for it, or says to use it.
  for (const act of [
    () => page.getByRole('textbox', { name: 'Prompt' }).fill('for the directory it showed'),
    async () => {
      await picked(page).click()
      await page.getByRole('button', { name: 'Use this directory' }).click()
    },
  ]) {
    let release = () => {}
    const held = new Promise<void>((r) => {
      release = r
    })
    await page.unrouteAll({ behavior: 'ignoreErrors' })
    await page.route(
      (url) => url.pathname === '/v2/dirs' && !url.searchParams.has('dir'),
      async (route) => {
        await held
        await route.fulfill({
          json: {
            schema: 'tmag/v2',
            places: {
              home: '/home/me',
              recent: [{ dir: '/elsewhere', at: '2026-11-18T00:00:00Z' }],
              templates: [{ name: 'later', agent: 'claude' }],
            },
          },
        })
      },
    )
    await page.goto('/#/start')
    await page.reload()
    // Until the computer says where its agents ran, it starts nothing.
    await expect(page.getByRole('button', { name: 'Start', exact: true })).toBeDisabled()
    await expect(picked(page)).toBeVisible()
    const shown = (await picked(page).getAttribute('aria-label')) ?? ''
    await act()
    release()
    // The places came: the template they name shows.
    await expect(page.getByRole('radio', { name: 'later · Claude Code' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Start', exact: true })).toBeEnabled()
    await expect(picked(page)).toHaveAttribute('aria-label', shown)
    expect(shown).not.toContain('/elsewhere')
  }
})

test('keeps the computer it first showed, though another answers later', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page, '', 'two')
  let release = () => {}
  const held = new Promise<void>((r) => {
    release = r
  })
  await page.route(
    (url) => url.pathname === '/v2/info',
    async (route) => {
      await held
      await route.continue()
    },
  )
  const asked: { path: string; dir: string }[] = []
  await page.route(
    (url) => url.pathname.endsWith('/v2/act'),
    (route) => {
      asked.push({ path: new URL(route.request().url()).pathname, dir: route.request().postDataJSON().dir })
      return route.fulfill({
        status: 409,
        json: { schema: 'tmag/v2', error: { code: 'refused:busy', message: 'not now' } },
      })
    },
  )
  await page.goto('/#/start')
  await expect(page.getByRole('radio', { name: 'two' })).toBeChecked()
  await picked(page).click()
  await page.getByRole('textbox', { name: 'Directory' }).fill(session(ops)?.cwd ?? '')
  release()
  await expect(page.getByRole('radio', { name: 'localhost' })).toBeVisible()
  await expect(page.getByRole('radio', { name: 'two' })).toBeChecked()
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await expect(page.getByRole('alert')).toBeVisible()
  expect(asked).toEqual([{ path: '/two/v2/act', dir: session(ops)?.cwd ?? '' }])
})

test('shows at the address it keeps what a link showed at another address of the same computer', async ({ page }) => {
  await paired(page)
  let release = () => {}
  const held = new Promise<void>((r) => {
    release = r
  })
  await page.route(
    (url) => url.pathname === '/v2/info',
    async (route) => {
      await held
      await route.continue()
    },
  )
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('again')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  await page.goto(`/#/again/s/${api.session}`)
  await expect(page.locator('header .name')).toHaveText('api')
  await page.getByRole('textbox', { name: 'Message' }).fill('written at the other address')
  // The first address says which computer it is: the same one.
  release()
  await expect(page.getByRole('alert')).toContainText('That is localhost, which the page already works with.')
  await expect(page).toHaveURL(new RegExp(`#/s/${api.session}$`))
  await expect(page.locator('header .name')).toHaveText('api')
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('written at the other address')
})

test('starts nothing once the computers change under the sheet, until the person starts over', async ({ page }) => {
  test.skip(!serves('start'), 'tmag starts agents after 0.7.0')
  await paired(page)
  let release = () => {}
  const held = new Promise<void>((r) => {
    release = r
  })
  await page.route(
    (url) => url.pathname === '/v2/info',
    async (route) => {
      await held
      await route.continue()
    },
  )
  await page.goto('/#/add')
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('again')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode())
  await page.getByRole('button', { name: 'Pair' }).click()
  const dir = encodeURIComponent(session(api)?.cwd ?? '')
  await page.goto(`/#/again/start/${dir}`)
  await page.getByRole('textbox', { name: 'Prompt' }).fill('for the computer it showed')
  // The first address says which computer it is: the one at again.
  release()
  await expect(page.getByText('The computers changed while this sheet was open.')).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`#/start/${dir}$`))
  await expect(page.getByRole('button', { name: 'Start', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Start over' }).click()
  await expect(picked(page)).toContainText(session(api)?.cwd ?? '')
  await expect(page.getByRole('textbox', { name: 'Prompt' })).toHaveValue('for the computer it showed')
})

test('works with no computer that does not say which it is', async ({ page }) => {
  await paired(page)
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => infoAs(route, { instance: undefined }),
  )
  await page.goto('/')
  await expect(page.locator('.bar')).toContainText('does not say which computer it is')
  await expect(page.locator('li.row:not(.skeleton)')).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'New agent', exact: true })).toHaveCount(0)
  await open(page, api)
  await expect(page.locator('main .empty')).toContainText('does not say which computer it is')
  await expect(page.getByRole('textbox', { name: 'Message' })).toHaveCount(0)
})

test('holds the next prompt while one is on its way, from whichever screen it went', async ({ page }) => {
  await paired(page)
  let release = () => {}
  const held = new Promise<void>((r) => {
    release = r
  })
  await page.route(
    (url) => url.pathname === '/v2/act',
    async (route) => {
      await held
      await route.continue()
    },
  )
  await page.goto('/')
  await tap(page, docs)
  await prompt(page, 'on its way')
  await back(page)
  await tap(page, docs)
  await page.getByRole('textbox', { name: 'Message' }).fill('the next one')
  await expect(page.getByRole('button', { name: 'Send' })).toBeDisabled()
  release()
  await expect(page.getByRole('button', { name: 'Send' })).toBeEnabled()
  await expect.poll(() => said(docs)).toContain('on its way')
})
