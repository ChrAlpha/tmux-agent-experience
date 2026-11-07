# ADR 0003: Local owner API and independent clients

- Status: accepted
- Date: 2026-11-07
- Supersedes: ADR 0001's remote access section.

Remote exploration tests whether the local foundation is sufficient. This
repository continues to deliver the tmux plugin, agent hooks, local state and
control, notification extension points, and an optional local HTTP API. UI,
mobile experiences, multi-machine inventory and SSH/container aggregation move
together to the independent `tmag-client` repository. Both repositories consume
versioned public contracts; neither imports the other's internal packages.

The HTTP API represents one owner. Every authorized client has the same owner
authority within the operating system privileges of the running tmag process.
The existing session, attachment, request and delivery gates still apply.
Process ancestry proves local CLI agent callers. HTTP admission instead carries
an opaque validated grant into the same query implementation; public input can
never set that identity. Control events record an optional credential id while
preserving `by` as the proven agent identity.

`tmag/http/v1` versions the transport independently of `tmag/v1` state documents.
The service listens on loopback and accepts explicit public hosts and browser
origins. Credentials authenticate requests independently of any tunnel vendor.
External deployment supplies encrypted remote transport and any extra identity
checks. The tmux window owns serve's lifecycle; no background service is installed.

Each client receives a random bearer token. The persistent authorization store
holds only token digests and metadata, under one stable instance id. A local
owner can issue, list and revoke credentials or enable a short, single-use
pairing invitation. Each redemption issues a new credential. Revocation closes
subscriptions, while previously admitted actions finish within their bound.
Authorization is independent of boot/PID realms; deployments sharing a home
select distinct `TMAG_AUTH_DIR` paths unless they intend to share authorization.

Notification events and the local command extension stay here. Transport-specific
push configuration and UI routes belong to clients. Local presence can suppress
the notification command, while the machine event stream remains complete.

The extracted client initially preserves its own browser admission and tunnel
adapters. Those protect that client's gateway and are separate from credentials
issued by the local tmag HTTP API.
