# tmag HTTP contract: tmag/http/v1

This contract is independent of client UI and release cadence. Paths start with
`/v1/`; responses carry `Tmag-API-Schema: tmag/http/v1`. A breaking HTTP change
requires a new path/schema version. Additive fields are allowed. State and CLI
operation documents retain their own `schema: tmag/v1` and existing semantics.

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
- Control refusal: 409, with the original `tmag/v1` refusal code.
- Local command usage: 400; timeout: 504; other command error: 422.
- A local runner returning no valid JSON: 502.

HTTP-layer errors have `{schema:"tmag/http/v1",error:{code,message}}`.
Successful commands and command failures preserve their `tmag/v1` documents.

## Resources

| Method/path | Input | Result |
| --- | --- | --- |
| GET /v1/info | none | HTTP schema, state schema, binary version, stable instance id, supported operation names |
| GET /v1/list | none | `list --json` |
| GET /v1/inspect | target | `inspect --json` |
| GET /v1/request | target, request | `request --json` |
| GET /v1/read | target and either last=1 or lines=1..2000 | `read --json` |
| GET /v1/watch | optional since cursor, otherwise Last-Event-ID; optional resync=1 | event stream |
| POST /v1/act | JSON action | send, interrupt, or answer result |
| POST /v1/pair | JSON `{code}`; no bearer needed | `{schema,credential:{id,label,created,expires?,token}}` |

Actions contain `verb` and `target`. A send carries `text`; an answer carries
`request` and `decision` (`allow` or `deny`), with optional `message` and
`answers` (question text to selected answer). Unknown body fields are rejected.
There is no machine selector, command string, sender identity or arbitrary shell.
Bodies are limited to 1 MiB and ten seconds to read. Control authority is
revalidated after the complete body and verb are checked; a stalled upload
cannot retain revoked or expired authority. Targets after the CLI's `--` remain literal.

A local `tmag auth pair LABEL` invitation expires after ten minutes and can be
redeemed once. Its optional `--ttl` controls the issued credential's lifetime.
Invalid, consumed or expired invitations return 403 `pairing`. Issuance returns
the secret exactly once; listing returns only metadata. CLI issuance/listing
use the existing `tmag/v1` CLI envelope; the HTTP pairing envelope is HTTP v1.

## Streaming and cancellation

SSE `event: doc` carries exactly one unchanged `tmag/v1` watch document in data.
Snapshot and event frames also carry an SSE id equal to their sequence number.
Pane-list frames do not advance the cursor. `event: ping` with data `1` is a
keepalive, not state. Reconnect with the last snapshot/event cursor. The local
watch contract replays missed events or starts a fresh snapshot after rotation.
Clients should tolerate duplicate state events, as the CLI contract requires.
If a running watch loses retained history, it sends the existing error document
with code `history_unavailable` and ends the subscription. Start a fresh watch
without a cursor and replace local state from its snapshot. `integrity_loss`
reports a malformed complete log record and requires explicit maintenance.
Neither error permits treating a retained suffix as a complete continuation.
`resync=1` opts into replacement snapshots during that subscription. Each
snapshot replaces both sessions and panes, including when either is empty.
At most three replacement attempts are allowed in thirty seconds; sustained
churn ends with the same terminal error document. Without this parameter,
midstream history loss remains terminal. SSE ids stay numeric, and snapshots
retain the CLI's replay-overlap semantics rather than claiming an atomic view.

Disconnecting ends that subscription and its local watch. Every second, serve
checks the persisted authorization store: revocation, expiry or a failed check
ends the subscription, including when another process revoked the credential.
A store check is bounded to five seconds. Subsequent requests authenticate again.

An admitted control operation runs to completion, bounded to two minutes, even
if its requester disconnects or its credential is subsequently revoked. Reads
cancel with the request. Server shutdown waits for admitted operations. A lost
response is an unknown outcome; clients reconcile state instead of automatically
retrying a control action. Subscriptions and content are never persisted by serve.
