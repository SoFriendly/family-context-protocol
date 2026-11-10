# Contributing to FCP

Thanks for helping. FCP aims to stay small: one static binary, no database, almost no dependencies, and nothing stored that doesn't have to be. Changes that keep it that way are the easiest to accept.

## Getting started

```sh
git clone https://github.com/sofriendly/family-context-protocol
cd family-context-protocol
go test ./...                 # unit and end-to-end tests
go build ./cmd/fcp
./scripts/smoke.sh ./fcp      # MCP over curl, the REST loop, "nothing left on the server"
node scripts/test-site.mjs    # landing-page checks
```

You need Go 1.26 or newer. The smoke test needs `curl` and `python3`.

## Pull requests

- Keep each PR focused, and include a test that fails without your change.
- Run `gofmt`, `go vet ./...`, and the tests before opening it.
- **Agent-facing text:** `internal/server/agents.md` (the rules agents follow) and `internal/server/join.md` (the setup instructions) are read by AI assistants. Write for an agent: concrete steps, exact field names, no ambiguity.
- **Changing the rules:** a change to `agents.md` changes `contract_version`. On the first start after an update, connected agents get a system message built from `ReleaseNote` in `internal/server/server.go`, so update that note to describe what changed.

## Layout

| Path | What's there |
|---|---|
| `cmd/fcp` | The CLI: `serve`, `update`, `health`, `version` |
| `internal/server` | HTTP routing, auth, rate limits, the REST API, setup links, attachments |
| `internal/mcp` | The MCP endpoint, built on the official Go SDK |
| `internal/store` | The keys file, plus in-memory inboxes, files, invites, and system messages |
| `internal/notify` | Webhook delivery |
| `internal/update` | Signed self-update |
| `internal/ui` | The admin page |
| `site/` | The getfcp.com landing page, a Cloudflare Worker with static assets |

## Releases

Maintainers run `./scripts/deploy-site.sh vX.Y.Z`. It builds all five platform binaries and signs `SHA256SUMS` with the release key, which never lives in the repo. It then publishes everything to getfcp.com.

**Forks:** releases you build yourself need your own signing key. Run `go run ./scripts/sign keygen`, put the public key in `internal/update/update.go`, and point `update.BaseURL` at your own download location.

## License

By contributing, you agree that your contributions are licensed under the MIT License.
