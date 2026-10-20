#!/usr/bin/env bash
# Build wyk for every supported architecture into OUT/<arch>/.
# The same flags in CI and in releases keep the binaries reproducible.
# Usage: scripts/build.sh [VERSION] [OUT]
set -euo pipefail

version=${1:-dev}
out=${2:-dist}
[[ $version =~ ^[A-Za-z0-9._+-]+$ ]] || { echo "bad version: $version" >&2; exit 1; }

# wyk-host.exe embeds the relay of its own architecture. Removed afterwards,
# so a plain go build cannot pick up one built for another.
embed=cmd/wyk-host/relay/wyk-relay
trap 'rm -f "$embed"' EXIT

export CGO_ENABLED=0
flags=(-trimpath -ldflags "-s -w -buildid= -X main.version=$version")
for arch in amd64 arm64; do
    GOOS=linux GOARCH=$arch go build "${flags[@]}" -o "$out/$arch/wyk-relay" ./cmd/wyk-relay
    cp "$out/$arch/wyk-relay" "$embed"
    GOOS=windows GOARCH=$arch go build "${flags[@]}" -o "$out/$arch/wyk-host.exe" ./cmd/wyk-host
done
