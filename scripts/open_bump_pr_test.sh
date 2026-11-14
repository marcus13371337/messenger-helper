#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
mkdir "$dir/bin"
cat > "$dir/bin/git" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  fetch) ;;
  show) printf 'module x\n\nrequire go.mau.fi/mautrix-meta %s\n' "$FAKE_BRANCH_VERSION" ;;
  *) echo "git $*" >> "$LOG" ;;
esac
STUB
cat > "$dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
if [ "$1 $2" = "pr list" ]; then echo "$FAKE_PR"; exit; fi
echo "gh $*" >> "$LOG"
STUB
chmod +x "$dir/bin/git" "$dir/bin/gh"

run() {
  local passed="$1" pr="$2" branch_version="$3"
  : > "$dir/log"
  PATH="$dir/bin:$PATH" LOG="$dir/log" FAKE_PR="$pr" FAKE_BRANCH_VERSION="$branch_version" \
    scripts/open-bump-pr.sh v0.2.0 "$passed" main https://example.com/run > /dev/null
}

expect() {
  local name="$1" want="$2"
  grep -q -- "$want" "$dir/log" || { echo "$name: no '$want' in:"; cat "$dir/log"; exit 1; }
}

expect_not() {
  local name="$1" unwanted="$2"
  if grep -q -- "$unwanted" "$dir/log"; then echo "$name: unexpected '$unwanted' in:"; cat "$dir/log"; exit 1; fi
}

run true "" ""
expect "new PR" "git push --force origin bump/mautrix-meta"
expect "new PR" "gh pr create"
expect_not "new PR" "--draft"

run false "" ""
expect "new failing PR" "gh pr create"
expect "new failing PR" "--draft"

run true "7 true" v0.1.0
expect "draft PR, newer release passes" "git push --force"
expect "draft PR, newer release passes" "gh pr edit 7"
expect "draft PR, newer release passes" "gh pr ready 7"

run false "7 false" v0.1.0
expect "ready PR, newer release fails" "gh pr ready --undo"

run false "7 true" v0.2.0
expect_not "PR already on this release" "git push"
expect_not "PR already on this release" "gh pr"

echo "open bump pr ok"
