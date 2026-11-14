#!/usr/bin/env bash
set -euo pipefail

upstream="${1:?usage: scripts/sync-replaces.sh <upstream go.mod> [go.mod, default ./go.mod]}"
gomod="${2:-go.mod}"

module_ref='if .Version then "\(.Path)@\(.Version)" else .Path end'
args=()
while read -r old; do
  args+=("-dropreplace=$old")
done < <(go mod edit -json "$gomod" | jq -r ".Replace // [] | .[].Old | $module_ref")
while read -r replacement; do
  args+=("-replace=$replacement")
done < <(go mod edit -json "$upstream" | jq -r ".Replace // [] | .[] | \"\(.Old | $module_ref)=\(.New | $module_ref)\"")

[ ${#args[@]} -eq 0 ] || go mod edit "${args[@]}" "$gomod"
