#!/bin/sh
# Build release binaries and deploy getfcp.com (landing page + downloads).
#   ./scripts/deploy-site.sh v0.1.0
set -eu
V=${1:?usage: deploy-site.sh <version, e.g. v0.1.0>}
cd "$(dirname "$0")/.."
./scripts/release.sh "$V"
# ponytail: earlier versions are kept only if their files are still in site/public/dl locally.
mkdir -p "site/public/dl/$V"
cp dist/fcp-* dist/SHA256SUMS dist/SHA256SUMS.sig "site/public/dl/$V/"
echo "$V" > site/public/dl/LATEST   # what `fcp update` checks
printf '/dl/latest/* /dl/%s/:splat 302\n' "$V" > site/public/_redirects
cp internal/server/agents.md site/public/agents.md
cd site && npx --yes wrangler@4 deploy
