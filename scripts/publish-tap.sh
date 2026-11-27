#!/usr/bin/env bash
# Pushes one generated file to bitskyai/homebrew-tap.
#
#   publish-tap.sh <path-in-tap> <generated-file> <commit message>
#
# HOMEBREW_TAP_TOKEN (env) authenticates through an HTTP header supplied via
# GIT_CONFIG_* environment variables, so it never appears in a remote URL,
# .git/config or a process argument list. Prerelease tags (TAG contains "-")
# are never published. Retries on a concurrent push from the other workflow.
set -euo pipefail
dest="$1" src="$2" msg="$3"
: "${HOMEBREW_TAP_TOKEN:?HOMEBREW_TAP_TOKEN is required}"
if [[ "${TAG:-}" == *-* ]]; then
  echo "prerelease $TAG: not publishing $dest to the tap"
  exit 0
fi
auth="$(printf 'x-access-token:%s' "$HOMEBREW_TAP_TOKEN" | base64 | tr -d '\n')"
echo "::add-mask::$auth"
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=http.https://github.com/.extraheader
export GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $auth"
export GIT_TERMINAL_PROMPT=0
tap="$(mktemp -d)"
trap 'rm -rf "$tap"' EXIT
git clone --depth 1 https://github.com/bitskyai/homebrew-tap.git "$tap"
git -C "$tap" config user.name 'github-actions[bot]'
git -C "$tap" config user.email '41898282+github-actions[bot]@users.noreply.github.com'
mkdir -p "$tap/$(dirname "$dest")"
cp "$src" "$tap/$dest"
git -C "$tap" add "$dest"
if git -C "$tap" diff --cached --quiet; then
  echo "$dest unchanged"
  exit 0
fi
git -C "$tap" commit -q -m "$msg"
for attempt in 1 2 3 4 5; do
  git -C "$tap" push origin HEAD:main && exit 0
  sleep $((attempt * 5))
  git -C "$tap" pull --rebase -q origin main
done
echo "could not push $dest to the tap" >&2
exit 1
