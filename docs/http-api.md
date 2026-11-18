# tmag HTTP contract: tmag/http/v2

This contract is independent of client UI and release cadence. Paths start with
`/v2/`; responses carry `Tmag-API-Schema: tmag/http/v2`. A breaking HTTP change
requires a new path/schema version. Additive fields are allowed. State and CLI
operation documents retain their own `schema: tmag/v2` and its semantics.

## Admission and deployment

Serve binds loopback. A remote deployment provides encrypted transport and an
explicit allowed Host (`--host`, repeatable, including a port when present).
Browser requests must match the listener's origin (scheme, host and port) or
an exact `--origin`. A TLS-terminating reverse proxy lists its public HTTPS
origin explicitly. CORS permits
Authorization, Content-Type and Last-Event-ID for GET/POST/OPTIONS. CORS and Host
checks are browser/deployment restrictions, not proof of the owner.

Every data request carries `Authorization: Bearer <token>`. All credentials
represent the owner, with the same authority and all the common operation gates.
Local `tmag auth` management issues invitations or revokes credentials;
proven agent tools are refused credential mutations and retain agent authority.
Bearer tokens and pairing codes are secrets; clients protect them in transit
and storage. Tokens are delivered only at issuance and are never put in URLs.

- Missing, expired or revoked credential: 401 with a Bearer challenge.
- Disallowed host or origin: 403.
- Unreadable authorization store: 503, distinct from invalid credentials.
- Body errors: 400; a non-JSON content type: 415.
- Control refusal: 409, with the original `tmag/v2` refusal code.
- Local command usage: 400; timeout: 504; other command error: 422.
- A local runner returning no valid JSON: 502.

HTTP-layer errors have `{schema:"tmag/http/v2",error:{code,message}}`.
Successful commands and command failures preserve their `tmag/v2` documents.

## Resources

| Method/path | Input | Result |
| --- | --- | --- |
| GET /v2/info | none | HTTP schema, state schema, binary version, stable instance id, supported operation names |
| GET /v2/list | none | `list --json` |
| GET /v2/inspect | target | `inspect --json` |
| GET /v2/request | target, request | `request --json` |
| GET /v2/changes | target, optional file | `changes --json`: what the agent's directory changed against HEAD, or one file's patch |
| GET /v2/read | target and one of last=1, lines=1..2000 or turns=1..20 | `read --json`; with lines, `ansi` and `cursor` show the screen and `screen` names it for keys |
| GET /v2/watch | optional since cursor, otherwise Last-Event-ID | event stream |
| POST /v2/act | JSON action | send, interrupt, answer, start or keys result |
| POST /v2/pair | JSON `{code}`; no bearer needed | `{schema,credential:{id,label,created,expires?,token}}` |

Actions contain `verb` and `target`. A send carries `text`; an answer carries
`request` and `decision` (`allow` or `deny`), with optional `message` and
`answers` (question text to selected answer). A send delivers now: an idle
session starts a turn, a working one reads the prompt at its next step, and
the Codex daemon joins an active turn. A person's or an owner's prompt carries
no sender line.
Unknown body fields are rejected.
A start carries no target but `agent` (claude, codex, pi or omp) or
`template` (a name the person set as `@tmag-start-NAME`), an absolute `dir`,
and optionally `worktree` (a branch) and `text`: it opens the agent's window
beside serve's, waits until it shows its session, and delivers `text` as a
send does. One whose agent shows no session in time fails with `pane`, the
pane its window keeps, beside the error: a read with lines and keys reach
it there.
Keys carry `target`, `screen` (as a read with lines gave it), and
`keys` (Enter, Escape, Tab, BTab, Up, Down, Left, Right, Space, BSpace, C-c,
y, n, 0-9) after an optional `text`: they are typed only once a shot of the
pane right before matches that screen.
There is no machine selector, command string, sender identity or arbitrary shell.
Bodies are limited to 1 MiB and ten seconds to read. Control authority is
revalidated after the complete body and verb are checked; a stalled upload
cannot retain revoked or expired authority. Targets after the CLI's `--` remain literal.

A local `tmag auth pair LABEL` invitation expires after ten minutes and can be
redeemed once. Its optional `--ttl` controls the issued credential's lifetime.
Invalid, consumed or expired invitations return 403 `pairing`. Issuance returns
the secret exactly once; listing returns only metadata. CLI issuance/listing
use the `tmag/v2` CLI envelope; the HTTP pairing envelope is HTTP v2.

## Streaming and cancellation

SSE `event: doc` carries exactly one unchanged `tmag/v2` watch document in data.
Snapshot and event frames also carry an SSE id equal to their sequence number.
Pane-list frames do not advance the cursor. `event: ping` with data `1` is a
keepalive, not state. Reconnect with the last snapshot/event cursor: the local
watch replays the events after it when the log still holds every one of them,
and otherwise starts with a fresh snapshot. A running subscription likewise
sends a fresh snapshot whenever it cannot vouch that it sent every event since
the last, because events rotated away or the log was recreated. Each snapshot
replaces both sessions and panes, including when either is empty. Events
report a session in full, so clients tolerate events they already applied,
including those a snapshot already shows; a snapshot is not an atomic view
across sessions and panes.

Disconnecting ends that subscription and its local watch. Every second, serve
checks the persisted authorization store: revocation, expiry or a failed check
ends the subscription, including when another process revoked the credential.
A store check is bounded to five seconds. Subsequent requests authenticate again.

An admitted control operation runs to completion, bounded to two minutes, even
if its requester disconnects or its credential is subsequently revoked. Reads
cancel with the request. Server shutdown waits for admitted operations. A lost
response is an unknown outcome; clients reconcile state instead of automatically
retrying a control action. Subscriptions and content are never persisted by serve.
