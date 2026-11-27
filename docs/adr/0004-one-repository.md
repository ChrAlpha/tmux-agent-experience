# ADR 0004: One repository for tmag and the products built on it

- Status: accepted
- Date: 2026-11-27
- Supersedes: ADR 0003's clause that moves clients to an independent
  repository.

tmag-client, the page for a phone, and tmag-flow, the runner of plans
written as code, live in this repository under `packages/`, with the
histories they had in repositories of their own. Every change they needed
from tmag was a change to its contract followed by one to theirs, and their
tests built tmag from this repository's main to learn, a day later, what a
change had broken. Here one change carries a contract and its consumers,
and the consumers' tests run on that change.

The boundary ADR 0003 drew stays. A package reaches tmag only through its
public contracts: the client through `tmag/http/v2` and the `tmag/v2`
documents it relays, tmag-flow through the CLI's `tmag/v2`. tmag imports
nothing of them, a package nothing of tmag's code, and each keeps its own
promises in its own AGENTS.md. Their end-to-end tests run against tmag of
the same commit and against the oldest release each supports, so a package
keeps working with the tmag people have installed.

tmag stays at the root, where TPM, the Go module path and the marketplaces
expect it. The packages share one pnpm workspace in `packages/`, which Go
tools ignore. tmag-flow's Claude Code and Codex plugins live beside tmag's
under `plugins/claude` and `plugins/codex`, the paths the marketplaces
`tmag setup` adds check out, and `tmag setup` installs only tmag's plugins.
TPM's checkout carries the packages' sources; the tmux plugin installs
nothing of them.

Each package has its own version and releases on tags of its own,
`client/vX.Y.Z` and `flow/vX.Y.Z`. Their releases are never marked latest:
latest is tmag's, which installers and the landing page's workflow look up.
One `ci` check covers the repository, and a package's jobs run when it or
tmag's code changes.
