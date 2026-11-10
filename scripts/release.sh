#!/bin/sh
# Cross-compile the release binaries into dist/ (static, no CGO).
#   ./scripts/release.sh v1.0.0
set -eu
V=${1:-dev}
mkdir -p dist
# linux/arm is built for ARMv6 (GOARM=6), so one binary covers every Raspberry Pi
# on a 32-bit OS, from the Pi Zero/1 up. GOARM is ignored for other targets.
for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 linux/arm; do
  os=${t%/*} arch=${t#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=6 go build -trimpath -ldflags "-s -w -X main.version=$V" -o "dist/fcp-$os-$arch" ./cmd/fcp
done
(cd dist && shasum -a 256 fcp-* > SHA256SUMS)
go run ./scripts/sign sign dist/SHA256SUMS   # dist/SHA256SUMS.sig, checked by `fcp update`
ls -l dist
