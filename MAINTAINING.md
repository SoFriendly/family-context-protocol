# Maintaining FCP

Notes for maintainers: releasing binaries and running getfcp.com.

## Releasing

```sh
./scripts/deploy-site.sh vX.Y.Z
```

This script:

1. Builds all five binaries (`scripts/release.sh`).
2. Writes `SHA256SUMS` and signs it with the release key.
3. Publishes the binaries under `/dl/vX.Y.Z/`.
4. Points `/dl/LATEST` and `/dl/latest/` at that version.
5. Copies `internal/server/agents.md` to `/agents.md`.
6. Runs `wrangler deploy`.

Before releasing:

- **Release note:** if `internal/server/agents.md` changed, update `ReleaseNote` in `internal/server/server.go`. Connected agents receive it as a system message on the first start after updating.
- **Working tree:** commit or set aside site changes. The deploy publishes `site/public` exactly as it is on disk.
- **Wrangler login:** log in to wrangler for the Cloudflare account that owns the `getfcp.com` zone.

Tag the release commit `vX.Y.Z` and push the tag.

## The release signing key

- **Location:** `~/.config/fcp/release-signing.key` on the maintainer's machine, or wherever `FCP_SIGNING_KEY` points. It's never in the repo.
- **What uses it:** `fcp update` and the admin update button install a release only if `SHA256SUMS.sig` verifies against the public key in `internal/update/update.go`.
- **Back it up in a password manager.** If it's lost, existing servers can't verify new releases, and every operator would have to download one release by hand.
- **New key:** `go run ./scripts/sign keygen` creates one. Put the printed public key in `internal/update/update.go`.

## getfcp.com

The site is a Cloudflare Worker with static assets (`site/`). It's not an FCP server, and FCP binaries never contact it except for update checks someone starts.

| URL | Serves |
|---|---|
| `https://getfcp.com/` | The landing page (`site/public/index.html`) |
| `https://www.getfcp.com/*` | A 301 redirect to the apex (`site/worker.js`) |
| `https://getfcp.com/dl/<version>/<file>` | Pinned downloads, `SHA256SUMS`, and `SHA256SUMS.sig` |
| `https://getfcp.com/dl/latest/<file>`, `/dl/LATEST` | The newest version |
| `https://getfcp.com/agents.md` | The agent contract |

- **Downloads:** these live in `site/public/dl/`, which is gitignored. Older versions stay published only if their files are still there locally.
- **Images:** the share image and icons come from `site/src/`; regenerate them with `./scripts/site-images.sh`.
- **Site tests:** `node scripts/test-site.mjs` checks the landing page.

Cloudflare setup:
- **Done:** `wrangler deploy` attached the custom domains (`getfcp.com`, `www.getfcp.com`) from `site/wrangler.jsonc`.
- **Recommended, in the dashboard:** set SSL/TLS to **Full (strict)** and turn on **Always Use HTTPS**.
- **Keep Web Analytics off,** because the site promises no analytics.
