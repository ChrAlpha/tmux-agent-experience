# tmag-client

This independent client consumes released tmag CLI/JSON and HTTP contracts. Keep
UI, browser admission, tunnel adapters, machine configuration and aggregation
here. Agent detection, session state, transcripts and control gates belong to the
local tmag instance. Never import another repository's internal Go packages.

Use the Go standard library and existing vanilla browser assets. Preserve the
upstream MIT attribution. Keep browser/gateway credentials distinct from upstream
HTTP credentials. Do not print secrets in test output or store tool input,
prompts, transcripts or credentials in repository fixtures.

Tests use temporary files, fake command backends and httptest servers. Any tmux
check must use a private server and temporary TMUX_TMPDIR. Never affect the user's
running server. Run go test ./..., go test -race ./..., golangci-lint run and
node --check internal/gateway/assets/app.js. Commits use Conventional Commits.
