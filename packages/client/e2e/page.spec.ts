import { appendFileSync } from 'node:fs'
import { expect, type Page, test } from '@playwright/test'
import type { Credential } from '../src/contract'
import {
  type Agent,
  type At,
  addTurn,
  agents,
  credential,
  pairingCode,
  person,
  said,
  serves,
  session,
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

const open = (page: Page, a: Agent) => page.goto(`/#/${a.at ? `${a.at}/` : ''}s/${a.session}`)

/** An agent's row in the list, whatever other rows say of it. */
const row = (page: Page, a: Agent) => page.locator(`.row[href$="s/${a.session}"]`)

async function idle(a: Agent) {
  await expect.poll(() => session(a)?.state, { timeout: 15_000 }).toBe('idle')
}

async function prompt(page: Page, text: string) {
  await page.getByRole('textbox', { name: 'Message' }).fill(text)
  await page.getByRole('button', { name: 'Send' }).click()
}

test.afterEach(async () => {
  for (const a of Object.values(agents)) await idle(a)
})

test('pairs with a code and opens the session it was sent to', async ({ page }) => {
  const refused: string[] = []
  page.on('console', (m) => m.text().includes('Content Security Policy') && refused.push(m.text()))
  await open(page, api)
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible()
  await page.getByRole('textbox', { name: 'Code' }).fill('WRONGCODE')
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.getByRole('alert')).toHaveText('That code is wrong, used or expired: run tmag auth pair again.')

  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode().toLowerCase())
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(page.locator('header .name')).toHaveText('api')
  await page.getByRole('button', { name: 'Agents' }).click()
  for (const name of ['api', 'web', 'docs']) await expect(page.locator('.row .name', { hasText: name })).toBeVisible()
  await expect(page.locator('footer')).toContainText('This device: e2e')
  // The page loads nothing its policy refuses, and its icons are its own.
  expect(refused).toEqual([])
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
  await expect(page.locator('footer')).toContainText('This device: e2e')
})

test('goes back to the list as the browser would', async ({ page }) => {
  await paired(page)
  // A session opened from a link goes back to the list in its place.
  await open(page, web)
  await expect(page.locator('header .name')).toHaveText('web')
  const entries = await page.evaluate(() => history.length)
  await page.getByRole('button', { name: 'Agents' }).click()
  await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible()
  expect(await page.evaluate(() => history.length)).toBe(entries)
  // One opened from the list goes back to it, and forward to it again.
  await row(page, api).click()
  await expect(page.locator('header .name')).toHaveText('api')
  await page.getByRole('button', { name: 'Agents' }).click()
  await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible()
  await page.goForward()
  await expect(page.locator('header .name')).toHaveText('api')
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
  page.on('dialog', (d) => void d.accept())
  await page.getByRole('button', { name: 'Agents' }).click()
  await page.getByRole('button', { name: 'Forget' }).click()
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
    (route) => route.fulfill({ json: { schema: 'tmag/http/v2', version: '0.6.0' } }),
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

  await page.getByRole('link', { name: 'Add a computer' }).click()
  await page.getByRole('checkbox').check()
  await page.getByRole('textbox', { name: 'Name' }).fill('two')
  await page.getByRole('textbox', { name: 'Code' }).fill(pairingCode('two'))
  await page.getByRole('button', { name: 'Pair' }).click()
  await expect(row(page, ops).locator('.name small')).toHaveText(' · two')
  await expect(row(page, api).locator('.name small')).toHaveText(' · localhost')
  await expect(page.locator('footer .device')).toHaveCount(2)

  await row(page, ops).click()
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

  page.on('dialog', (d) => void d.accept())
  await page.goto('/')
  await page.locator('footer .device', { hasText: 'two:' }).getByRole('button', { name: 'Forget' }).click()
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
  await expect(apiRow.locator('.meta')).toContainText('claude · done')
  await expect(page).toHaveTitle('(1) tmag')
  await apiRow.click()
  await expect(page.locator('.reply').last()).toHaveText('answered: done while away')
  await page.getByRole('button', { name: 'Agents' }).click()
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
  await page.getByRole('button', { name: 'Agents' }).click()
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
  await page.goto(`/#/s/${api.session}/r/${request}`)
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
  await page.getByRole('button', { name: 'Agents' }).click()
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
  await page.getByRole('button', { name: 'Agents' }).click()
  await expect(row(page, web)).toHaveClass(/done/)

  await row(page, web).click()
  await person(`tmag send ${web.session} 'while away again'`)
  await expect(page.locator('.reply').last()).toHaveText('answered: while away again', { timeout: 15_000 })
  await hide(false)
  await page.getByRole('button', { name: 'Agents' }).click()
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
  await expect(page.locator('footer .device')).toHaveCount(2)
})

test('offers only what the computer serves', async ({ page }) => {
  await paired(page)
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) =>
      route.fulfill({
        json: { schema: 'tmag/http/v2', version: '0.7.0', operations: ['list', 'inspect', 'watch', 'read', 'request'] },
      }),
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
  await expect(page.locator('footer .device')).toHaveCount(1)
  await expect(row(page, api)).toHaveCount(1)
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
  await expect(page.locator('footer .device')).toHaveCount(1)
  await expect(row(page, api)).toHaveCount(1)
})

test('reads nothing of a session its computer does not serve', async ({ page }) => {
  await paired(page)
  const reads: string[] = []
  page.on('request', (r) => new URL(r.url()).pathname === '/v2/read' && reads.push(r.url()))
  await page.route(
    (url) => url.pathname === '/v2/info',
    (route) => route.fulfill({ json: { schema: 'tmag/http/v2', version: '0.7.0', operations: ['list', 'watch'] } }),
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
  await page.getByRole('button', { name: 'Agents' }).click()
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
    (route) =>
      route.fulfill({ json: { schema: 'tmag/http/v2', version: '0.7.0', operations: ['list', 'watch', 'send'] } }),
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
  await page.getByRole('button', { name: 'Agents' }).click()
  await row(page, api).click()
  // The pairing ends once the computer it added is watched.
  const watched = page.waitForRequest((r) => new URL(r.url()).pathname === '/two/v2/watch')
  hold = false
  await watched
  await expect(page).toHaveURL(new RegExp(`#/s/${api.session}$`))
})
