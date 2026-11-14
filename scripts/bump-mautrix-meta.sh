#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

module=go.mau.fi/mautrix-meta
version="$(go list -m -f '{{.Version}}' "$module@${1:-latest}")"

upstream_mod="$(go mod download -json "$module@$version" | jq -er .GoMod)"
scripts/sync-replaces.sh "$upstream_mod"
go get "$module@$version"
go mod tidy
echo "$version"
