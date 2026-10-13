#!/usr/bin/env bash
# Fails when openapi.yaml breaks clients relative to BASE (a git rev) without bumping info.x-api-version.
# oasdiff sees paths, parameters and schemas only; an x-sse-events change needs a hand bump (docs/api.md).
set -euo pipefail
cd "$(dirname "$0")/.."
base=${1:?usage: check-api-compat.sh <base-rev>}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git show "$base:openapi.yaml" > "$tmp/base.yaml"

ver() { v=$(sed -n 's/^  x-api-version: *\([0-9][0-9]*\)$/\1/p' "$1"); echo "${v:-0}"; }
if go run github.com/oasdiff/oasdiff@v1.33.0 breaking "$tmp/base.yaml" openapi.yaml --fail-on ERR; then
  exit 0
fi
if [ "$(ver openapi.yaml)" -gt "$(ver "$tmp/base.yaml")" ]; then
  echo "breaking changes above ship with an x-api-version bump"
  exit 0
fi
echo "openapi.yaml has breaking changes (above) but info.x-api-version was not bumped; see docs/api.md" >&2
exit 1
