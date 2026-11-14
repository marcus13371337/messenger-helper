#!/usr/bin/env bash
set -euo pipefail

upstream="${1:?usage: scripts/sync-replaces.sh <upstream go.mod> [go.mod, default ./go.mod]}"
gomod="${2:-go.mod}"

module_ref='if .Version then "\(.Path)@\(.Version)" else .Path end'
ours="$(go mod edit -json "$gomod" | jq -r ".Replace // [] | .[].Old | $module_ref")"
theirs="$(go mod edit -json "$upstream" | jq -r ".Replace // [] | .[] | \"\(.Old | $module_ref)=\(.New | $module_ref)\"")"

args=()
while read -r old; do
  [ -z "$old" ] || args+=("-dropreplace=$old")
done <<< "$ours"
while read -r replacement; do
  [ -z "$replacement" ] || args+=("-replace=$replacement")
done <<< "$theirs"

[ ${#args[@]} -eq 0 ] || go mod edit "${args[@]}" "$gomod"
