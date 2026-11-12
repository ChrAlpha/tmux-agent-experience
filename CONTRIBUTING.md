# Contributing

Thanks for helping. tmag is small on purpose; the most useful contributions
keep it that way.

## Before you start

For anything beyond a bug fix, open an issue first. Proposals that add a
resident process, wrap the agents, or edit user configuration conflict with
the design (see the invariants in [AGENTS.md](AGENTS.md)) and will be declined.

## Development

You need Go (the version in `go.mod` or newer) and tmux 3.2 or newer. The
commands, layout and testing conventions are in [AGENTS.md](AGENTS.md); they
apply to people as much as to agents.

A change is ready when `go test -race ./...`, `golangci-lint run`, the
shellcheck command in [AGENTS.md](AGENTS.md#commands) and `claude plugin
validate .` pass, and behaviour changes are covered by a test. If you change
how a hook payload or terminal title is read, describe how you checked it
against the real agent and which version.

## Pull requests

- Use Conventional Commits for commit messages and the PR title.
- Keep one concern per PR.
- Update README.md when user-visible behaviour or the JSON contract changes.

## Releasing

1. Set the new version in `internal/version/version.go` and the eight plugin
   manifests, `plugins/{claude,codex,pi,omp}/{tmag,tmag-peers}/`
   (`.claude-plugin/plugin.json`, `.codex-plugin/plugin.json` or
   `package.json`).
2. Merge to `main`, then tag it: `git tag v0.2.0 && git push origin v0.2.0`.
3. The release workflow checks the tag against the version, builds Linux and
   macOS binaries and publishes the release with checksums and build
   provenance. Once it succeeds, the pages workflow publishes the landing page
   from the tagged commit.
4. Check the install paths users take, on Linux and macOS: the TPM install
   (`prefix + I` downloads the new binary, `prefix + M-a` connects the agents,
   Pi's and oh-my-pi's packages included) and the steps in `SKILL.md`.
