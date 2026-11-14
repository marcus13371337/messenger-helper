#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

cat > "$dir/upstream.mod" <<'MOD'
module go.mau.fi/mautrix-meta

go 1.26.0

replace github.com/imroc/req/v3 => github.com/beeper/req/v3 v3.0.0-20270101000000-aaaaaaaaaaaa

replace example.com/added v1.0.0 => example.com/fork v1.0.1
MOD

cat > "$dir/go.mod" <<'MOD'
module example.com/app

go 1.26.0

replace github.com/imroc/req/v3 => github.com/beeper/req/v3 v3.0.0-20260925135712-b49593750e56

replace example.com/removed => example.com/other v0.1.0
MOD

scripts/sync-replaces.sh "$dir/upstream.mod" "$dir/go.mod"

got="$(go mod edit -json "$dir/go.mod" | jq -c '[.Replace[] | [.Old.Path, .Old.Version, .New.Path, .New.Version]] | sort')"
want='[["example.com/added","v1.0.0","example.com/fork","v1.0.1"],["github.com/imroc/req/v3",null,"github.com/beeper/req/v3","v3.0.0-20270101000000-aaaaaaaaaaaa"]]'
test "$got" = "$want" || { echo "replaces: got $got, want $want"; exit 1; }

cp "$dir/go.mod" "$dir/before.mod"
if scripts/sync-replaces.sh "$dir/missing.mod" "$dir/go.mod" 2>/dev/null; then
  echo "a missing upstream go.mod did not fail"; exit 1
fi
cmp -s "$dir/go.mod" "$dir/before.mod" || { echo "a missing upstream go.mod changed go.mod"; exit 1; }

echo "sync replaces ok"
