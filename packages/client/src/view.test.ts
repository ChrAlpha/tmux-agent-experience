import { describe, expect, it } from 'vitest'
import type { Event, Session, Turn, WatchDoc } from './contract'
import {
  ago,
  apply,
  empty,
  find,
  follows,
  helpers,
  paneOf,
  type Sent,
  sessions,
  stoppable,
  unread,
  unwritten,
} from './view'

const session = (id: string, state: Session['state'], extra: Partial<Session> = {}): Partial<Session> => ({
  id,
  agent: 'claude',
  state,
  since: '2026-11-13T10:00:00Z',
  capabilities: ['send'],
  ...extra,
})
const event = (seq: number, name: string, s: string, extra: Partial<Event> = {}): WatchDoc => ({
  type: 'event',
  event: { seq, time: '2026-11-13T11:00:00Z', event: name, agent: 'claude', session: s, ...extra },
})

// The page rebuilds what `tmag list --json` reports from the watch alone, as
// the e2e follower of tmux-agent-experience does.
describe('apply', () => {
  it('follows a snapshot, events and a panes document', () => {
    let v = apply(empty, {
      type: 'snapshot',
      seq: 7,
      sessions: [session('a', 'idle'), session('b', 'working')],
      panes: [{ server: 's', id: '%1', session: 'a', agent: 'claude', target: 'main:1.0' }],
    })
    expect(v.sessions.size).toBe(2)
    expect(find(v, 'a')?.requests).toEqual([])
    expect(paneOf(v, find(v, 'a') as Session)?.target).toBe('main:1.0')

    expect(apply(v, event(8, 'Send', 'a')), 'an event without a state changes nothing').toBe(v)
    v = apply(
      v,
      event(9, 'PermissionRequest', 'a', {
        state: 'blocked',
        kind: 'permission',
        requests: [{ id: 'r1', kind: 'permission', tool: 'Bash' }],
      }),
    )
    expect(find(v, 'a')).toMatchObject({ state: 'blocked', since: '2026-11-13T11:00:00Z' })
    expect(find(v, 'a')?.requests[0]?.tool).toBe('Bash')

    const b = find(v, 'b')
    v = apply(v, event(10, 'PreToolUse', 'a', { state: 'blocked', kind: 'permission', time: '2026-11-13T11:05:00Z' }))
    expect(find(v, 'a')?.since, 'the same state keeps its start').toBe('2026-11-13T11:00:00Z')
    expect(find(v, 'a')?.requests, 'a field left out is empty').toEqual([])
    expect(find(v, 'b'), 'a session no event reports stays the same object').toBe(b)
    v = apply(v, event(11, 'Stop', 'a', { state: 'idle', since: '2026-11-13T11:06:00Z' }))
    expect(find(v, 'a')?.since, "an event's own since wins").toBe('2026-11-13T11:06:00Z')

    v = apply(v, event(12, 'SessionEnd', 'b'))
    expect(apply(v, event(13, 'Exit', 'c'))).toBe(v)
    expect([...v.sessions.keys()]).toEqual(['claude/a'])

    v = apply(v, { type: 'panes', panes: [] })
    expect(paneOf(v, find(v, 'a') as Session)).toBeUndefined()
    v = apply(v, { type: 'snapshot', seq: 20 })
    expect(v.sessions.size, 'a snapshot replaces every session').toBe(0)
  })
})

describe('sessions', () => {
  it('lists blocked, working, then idle, the latest change first', () => {
    const v = apply(empty, {
      type: 'snapshot',
      seq: 1,
      sessions: [
        session('idle-old', 'idle', { since: '2026-11-13T09:00:00Z' }),
        session('working', 'working'),
        session('idle-new', 'idle', { since: '2026-11-13T12:00:00Z' }),
        session('blocked', 'blocked'),
      ],
    })
    expect(sessions(v).map((s) => s.id)).toEqual(['blocked', 'working', 'idle-new', 'idle-old'])
  })
})

describe('helpers', () => {
  it('counts the subagents an event reports', () => {
    let v = apply(empty, { type: 'snapshot', seq: 1, sessions: [session('a', 'working')] })
    expect(helpers(find(v, 'a') as Session)).toBe('')
    v = apply(v, event(2, 'SubagentStart', 'a', { state: 'working', subagents: ['s1'] }))
    expect(helpers(find(v, 'a') as Session)).toBe('1 subagent')
    v = apply(v, event(3, 'SubagentStart', 'a', { state: 'working', subagents: ['s1', 's2'] }))
    expect(helpers(find(v, 'a') as Session)).toBe('2 subagents')
  })
})

describe('stoppable', () => {
  it("offers Stop where tmag's gate would carry an interrupt", () => {
    const s = (state: Session['state'], kind?: string, capabilities = ['interrupt']) =>
      ({ ...session('a', state, { kind, capabilities }), requests: [] }) as Session
    expect(stoppable(s('working'))).toBe(true)
    expect(stoppable(s('blocked', 'permission'))).toBe(true)
    expect(stoppable(s('working', 'background')), 'only background tasks run').toBe(false)
    expect(stoppable(s('idle'))).toBe(false)
    expect(stoppable(s('working', undefined, ['send'])), 'no channel carries an interrupt').toBe(false)
  })
})

describe('unread', () => {
  const turn = (...prompts: string[]): Turn => ({ id: 't', prompts: prompts.map((text) => ({ text })), reply: '' })
  const at = (state: Session['state'], since: string) =>
    ({ ...session('a', state, { since }), requests: [] }) as Session
  const sent: Sent = { id: 1, text: 'yes', seen: 1, since: '2026-11-13T10:00:00Z' }

  it('keeps a prompt until the turns show it once more than when it was sent', () => {
    const before = [turn('yes'), turn('go on')]
    expect(unread([sent], before, at('working', '2026-11-13T10:00:00Z'))).toEqual([sent])
    expect(unread([sent], [...before, turn(' yes\n')], at('working', '2026-11-13T10:00:00Z'))).toEqual([])
  })

  it('lets a prompt go once the session went idle since it was sent', () => {
    expect(unread([sent], [], at('idle', '2026-11-13T10:00:00Z')), 'still the idle it was sent in').toEqual([sent])
    expect(unread([sent], [], at('working', '2026-11-13T10:01:00Z'))).toEqual([sent])
    expect(unread([sent], [], at('idle', '2026-11-13T10:02:00Z')), 'the turn it started has ended').toEqual([])
    expect(unread([sent], [], undefined), 'a session that ended shows as such').toEqual([sent])
  })
})

describe('follows', () => {
  it('follows a prompt into the turns, not a slash command', () => {
    expect(follows('fix the test')).toBe(true)
    expect(follows(' /compact')).toBe(false)
  })
})

describe('unwritten', () => {
  it('says when the turn an idle session ended has no reply yet', () => {
    const at = (state: Session['state'], turn?: string) =>
      ({ ...session('a', state, { turn }), requests: [] }) as Session
    const turns: Turn[] = [
      { id: 't1', prompts: [], reply: 'done' },
      { id: 't2', prompts: [], reply: '' },
    ]
    expect(unwritten(at('idle', 't2'), turns)).toBe(true)
    expect(unwritten(at('working', 't2'), turns), 'a running turn writes on').toBe(false)
    expect(unwritten(at('idle', 't3'), turns), 'its turns do not hold it yet').toBe(false)
    expect(unwritten(at('idle', 't1'), turns.slice(0, 1))).toBe(false)
    expect(unwritten(at('idle'), turns)).toBe(false)
  })
})

describe('ago', () => {
  it('says how long ago, in its largest unit', () => {
    const now = Date.parse('2026-11-13T12:00:00Z')
    expect(ago('2026-11-13T11:59:55Z', now)).toBe('now')
    expect(ago('2026-11-13T11:59:00Z', now)).toBe('1m')
    expect(ago('2026-11-13T09:00:00Z', now)).toBe('3h')
    expect(ago('2026-11-10T12:00:00Z', now)).toBe('3d')
    expect(ago(undefined, now)).toBe('')
  })
})
