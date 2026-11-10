# tmag

Local agent awareness and control for the owner of a tmux environment.

## Language

**Owner**: The person authorizing clients to act on their local agents. All
owner-authorized clients have the same authority.

**Client credential**: One independently revocable authorization issued by the
owner. Its label is a convenience, not proof of a device or agent identity.

**Instance**: One local deployment with its own persistent owner authorization.

**Realm**: The process environment in which tmag can prove sessions and callers.
A new boot or isolated process environment creates a new realm.

**Gateway**: An external client's relay that aggregates multiple instances or
executes tmag through remote command transports.

**Inbox**: The Unix socket a session's own agent serves for messages. It is
`send`'s channel for Claude Code, Pi and oh-my-pi; the agent's queue holds the
message, and tmag keeps nothing.
