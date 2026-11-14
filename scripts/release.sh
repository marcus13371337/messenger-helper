#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

tag="${1:?usage: scripts/release.sh <tag, e.g. v0.1.0>}"
version="${tag#v}"

rm -rf dist
mkdir dist
for platform in darwin-arm64 darwin-x64 linux-x64 linux-arm64; do
  goos="${platform%-*}"
  goarch="${platform#*-}"
  [ "$goarch" = x64 ] && goarch=amd64
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "dist/messenger-helper-$platform" .
done
(cd dist && shasum -a 256 messenger-helper-* > checksums.txt)
