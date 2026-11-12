# ADR 0005: The person's seen turns

- Status: accepted
- Date: 2026-11-12
- Builds on: [ADR 0004](0004-remote-companion.md), whose `Alert` event and
  companion this extends; both are unmerged.
- Evidence: T3 Code (`pingdotgg/t3code`) settles threads on the server —
  "Thread settlement is server-owned ... Clients render the persisted result;
  they do not derive settlement from their own clocks or PR caches"
  (`docs/internals/overview.md` at `2482b03`) — and the three places tmag
  answered the same question differently, observed on 2026-11-12: `tmag jump`
  counted every idle session, the terminal's bell flag knew what the person had
  visited but nothing else read it, and the companion kept a tracker of seen
  turns with its own file, limit, expiry and presence. tmux 3.2a, 3.3a and 3.4
  were used to check the hooks this decision installs.

## Context

tmag answers one question in several places, and they disagree: "which agents
need me". `tmag jump` (`prefix + A`) counts every idle session as waiting for
the person, oldest first, so an agent whose answer the person already read
keeps winning over one that just finished. The terminal's bell flag (tmux
turns an alert's bell into a window flag that visiting clears; `prefix + M-n`)
knows what the person has visited, but nothing else reads it. The companion's
gateway kept its own seen turns, and seven review rounds kept finding the
lifecycle that tracker re-implemented: a quiet session, a racing request, a
crash or a removed machine losing or keeping seen state forever. A session
record already has that lifecycle. And a turn read at the terminal stayed in
the phone's inbox, while one read on the phone still won `prefix + A`.

A turn the person has seen is not content. It is one id, and the evidence for
it is the same evidence the bell already rests on: a watched pane, and a tmux
server that had the person's input recently.

## Decision

**S1. What seen is.** A session's record keeps `seen`: the id of the last turn
whose end the person has seen. A session needs the person when it is blocked,
or idle on a turn that is not its `seen` turn; such a turn is *finished*. An
idle session with no turn, or whose turn is its `seen` turn, is *settled*. Seen
belongs to the person: no agent marks a turn seen, and no read, wait, send or
answer marks one. When unsure, a turn stays unseen: showing a turn twice is
better than hiding one.

**S2. Seen from the terminal.** A turn is seen when, at its end or later, a
pane that shows its session (a proven attachment) is watched — the active pane
of the active window of an attached tmux session, `tmux.Pane.Watched` — on a
tmux server whose clients had the person's input within `notify.Present`. tmag
checks this in three places:

- in the hook that applies the turn's end, reusing the same `shown` panes and
  presence the `Alert` computes once, with no tmux call beyond them for a
  change that alerts and none at all for one that does not;
- when the view changes, through tmux hooks the plugin installs (S6), which
  run `tmag seen --shown`;
- in `tmag status`, which tmux already runs every `status-interval` for
  attached clients using the `#{tmag_status}` placeholder.

**S3. Seen by a person's command.** `tmag seen ID [--turn TURN]` marks TURN
(the session's current turn by default) seen when the session is idle on that
turn. An agent's call is refused, as `answer` refuses an agent unless
`@tmag-answer` is `agents`, but with no option: an agent may never mark a turn
seen. A session not idle on that turn is refused with the refusal the other
verbs use for a state that does not fit; a turn already seen is a quiet
success only while the session is still idle on it; every refusal prints a
`next:` line. `--json` prints what `interrupt
--json` prints for its delivery — the session's document, now with `seen`, and
the event's `seq` — with no event when nothing changed.

`tmag serve` exposes it to the owner as the control verb `seen` of
`POST /v2/act`: `{"verb":"seen","target":"ID","turn":"TURN"}`, with `turn`
optional, runs `seen --json [--turn=TURN] -- ID`; `GET /v2/info` lists `seen`
among the operations. The event records the credential, as control events do.
The companion's HTTP backends use exactly this operation.

**S4. Record and log.** `session.Record` gains `Seen string` (`json:"seen,omitempty"`),
bounded as turn ids are bounded where they are read: a `--turn` flag and an
HTTP action carry the same bound (`session.MaxTurn`, `adapter.MaxName`). It
changes only under the session's lock, decided again under it (still idle on
that turn, not yet seen), and is logged before the lock is released as a
`Seen` event (`eventlog.Seen`) carrying that turn in the existing top-level
`turn` field, and `credential` when through `tmag serve`. The hook that sets it
at a turn's end writes it in the same record write as the change, and logs
`Seen` after that change's event and its `Alert`. `tmag list --json`, snapshots
and `tmag inspect` show `seen` on the session; an event that reports a state
carries it too, and `tmag watch` carries `Seen` events. This is an optional
addition to `tmag/v2` and `tmag/http/v2`: no schema name changes. An older tmag
that rewrites a record drops `seen`, so its turn shows as finished again — the
convention for optional record fields, accepted.

**S5. What renders it in core.** `tmag jump` takes blocked sessions first,
oldest first, then finished ones, oldest first; settled sessions are never a
target; with none it reports `no agent is waiting for you` as before. Its key's
note stays "Jump to the agent waiting longest". `tmag status` counts, in this
order, `blocked`, `finished`, `working`, `idle`, where `finished` counts idle
sessions on an unseen turn and `idle` the settled ones; styling is unchanged
(blocked red and bold under `--tmux`). `tmag list` (text), the picker and
`@tmag-pane-status` keep the contract's states (`idle`, `working`, `blocked`,
`unknown`), which the `peers` skill teaches; neither the skill nor any agent's
verbs change.

**S6. The plugin's tmux hooks and option.** `tmag.tmux` sets `@tmag-seen`
(default `on`). On, the plugin installs global tmux hooks on the events by
which a pane comes into an attached client's view:

| Hook | Scope | When it fires |
| --- | --- | --- |
| `session-window-changed` | `set-hook -g` | a session's current window changes (`select-window`, `next-window`, `last-window`, a killed window) |
| `window-pane-changed` | `set-hook -gw` | a window's active pane changes (`select-pane`, `select-pane -l`, `last-pane`, the mouse) |
| `client-session-changed` | `set-hook -g` | a client switches session, including when it attaches |
| `client-attached` | `set-hook -g` | a client attaches |

A pane's hook is a global window member (`-gw`) because `window-pane-changed`
is a window notification, and the global window member applies to every
window; the others are server-global (`-g`).

Each runs `'$tmag' seen --shown --server '#{socket_path}'` in the background
(`run-shell -b`), behind an `if-shell -F` filter for the live expression
tmag.tmux already has (the pane's `@tmag_state` and `@tmag_cmd` match the pane's
current command) and `@tmag_state=idle`, so it runs only when the pane now in
view is a live agent pane that is idle. A hook is one member of tmux's hook
array at an index of tmag's own: it finds its own member by a marker on reload
and reuses it, or takes the first free index from 97 upward, so a person's
hooks keep theirs. Loading the plugin again replaces the member in place and
never stacks copies. `@tmag-seen off` removes tmag's members and installs none;
`tmag status` and `tmag seen --shown` then mark nothing, and only S3 does.

`tmag seen --shown [--server SOCKET]` marks every finished turn whose session a
watched pane on that server shows, when the server had the person's input
within `notify.Present`; it exits 0 whenever the server answers, within a short
deadline like `status`, and prints nothing. It is the plugin's command, like
`jump --client`.

**S7. The companion renders it too.** The inbox is blocked sessions, or idle
sessions with a turn different from `seen`; then working ones; then settled
ones (idle with no turn, or whose turn is `seen`); unknown otherwise. The
gateway keeps no session state: `tracker.go`, `seen.json`, its limits, expiry
and presence, the `seen` SSE frame and the snapshot's `seen` map go. Seen
arrives in session documents and `Seen` events, which the gateway's follower
applies like any other event and the page renders. Opening a session on the
page marks the turn it shows seen when that turn is finished, through
`POST /api/seen`, which runs `tmag seen ID --turn TURN` on that machine as a
person — a command backend appends the argv, an HTTP backend calls `tmag
serve`'s operation — and the page reflects the `Seen` event that follows. A
failure shows on the page. Push is unchanged: the `Alert`'s `present` decides,
and a finished alert whose turn is already seen when the push is about to go is
not pushed, since the current state contradicts it. A machine on an older tmag
has no `seen`: its finished turns stay in the inbox, and marking one fails with
that tmag's error. There is no fallback tracker. The client README says the
companion needs tmag 0.7.0 for push and seen.

## Consequences

A session record carries one more id, an event one more optional field, and
the contract one more optional operation. Nothing else changes shape: the
states in `list`, the picker and the pane border are the contract's, and seen
is a fact of the record that every surface renders, not a second state
machine. A tmag that predates this ADR drops `seen` when it rewrites a record;
the next view change or `status` marks it again, and until then the turn is
shown, which is the side to err on.
