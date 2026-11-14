#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

scripts/release.sh v1.2.3

for platform in darwin-arm64 darwin-x64 linux-x64 linux-arm64; do
  test -x "dist/messenger-helper-$platform" || { echo "missing dist/messenger-helper-$platform"; exit 1; }
done
test "$(ls dist | wc -l | tr -d ' ')" = 5 || { echo "unexpected files in dist:"; ls dist; exit 1; }
(cd dist && shasum -a 256 -c checksums.txt)

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) host=darwin-arm64 ;;
  Darwin-x86_64) host=darwin-x64 ;;
  Linux-x86_64) host=linux-x64 ;;
  Linux-aarch64) host=linux-arm64 ;;
esac
got="$("dist/messenger-helper-$host" --version)"
want="messenger-helper 1.2.3 (protocol 1)"
test "$got" = "$want" || { echo "--version printed '$got', want '$want'"; exit 1; }

echo "release build ok"
