#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:-latest}"
module=go.mau.fi/mautrix-meta

go get "$module@$version"
resolved="$(go list -m -f '{{.Version}}' "$module")"
scripts/sync-replaces.sh "$(go mod download -json "$module@$resolved" | jq -r .GoMod)"
go mod tidy
echo "$resolved"
