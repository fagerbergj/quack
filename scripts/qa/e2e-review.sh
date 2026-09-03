#!/usr/bin/env bash
# Fires a fixture GitHub webhook at a QA server, waits for the resulting
# chat to leave "running", then dumps its artifacts and the github-mock's
# recorded deliveries. Talks to the server only through the existing
# `quack api` CLI - see docs/qa-mocks.md.
#
# Usage: scripts/qa/e2e-review.sh --secret SECRET [--url URL] [--fixture FILE] [--chat ID]
#   --chat: if omitted, the script lists chats after sending and asks you
#   to re-run with the new one - there's no webhook-response chat id to key
#   off (quack replies 202 before the run is created).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gh_mock="$repo_root/../quack-extensions/github"
fixtures="$repo_root/testdata/qa/github"

url="http://localhost:8080/api/v1/github/webhook"
fixture="$fixtures/events/issues.labeled.quack-review.json"
secret=""
chat_id=""

while [ $# -gt 0 ]; do
  case "$1" in
    --secret) secret="$2"; shift 2 ;;
    --url) url="$2"; shift 2 ;;
    --fixture) fixture="$2"; shift 2 ;;
    --chat) chat_id="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$secret" ]; then
  echo "usage: e2e-review.sh --secret SECRET [--url URL] [--fixture FILE] [--chat ID]" >&2
  exit 2
fi

echo "== sending $fixture =="
( cd "$gh_mock" && go run ./cmd/qa-mock send \
    --fixture "$fixture" \
    --event issues \
    --secret "$secret" \
    --url "$url" )

if [ -z "$chat_id" ]; then
  echo
  echo "No --chat given - listing chats, find the new one and re-run with --chat <id>:"
  quack api /api/v1/chats
  exit 0
fi

echo "== polling chat $chat_id =="
timed_out=1
for _ in $(seq 1 60); do
  status=$(quack api "/api/v1/chats/$chat_id" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status","?"))')
  echo "status: $status"
  if [ "$status" != "running" ]; then
    timed_out=0
    break
  fi
  sleep 5
done
[ "$timed_out" -eq 1 ] && echo "still running after 5 min - checking current state anyway"

echo "== artifacts =="
quack api "/api/v1/chats/$chat_id/artifacts"

echo "== github-mock deliveries =="
( cd "$gh_mock" && go run ./cmd/qa-mock deliveries --fixtures "$fixtures" )
