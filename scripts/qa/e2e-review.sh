#!/usr/bin/env bash
# Fires the fixture quack:review webhook at a QA server, waits for the
# resulting chat to leave "running", then dumps its artifacts and the
# github-mock's recorded deliveries. Talks to the server only through the
# existing `quack api` CLI - see docs/qa-mocks.md.
#
# Usage: scripts/qa/e2e-review.sh <webhook-secret> [chat-id]
#   chat-id: if omitted, the script lists chats after sending and asks you
#   to re-run with the new one - there's no webhook-response chat id to key
#   off (quack replies 202 before the run is created).
set -euo pipefail

secret="${1:?usage: e2e-review.sh <webhook-secret> [chat-id]}"
chat_id="${2:-}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gh_mock="$repo_root/../quack-extensions/github"
fixtures="$repo_root/testdata/qa/github"

echo "== sending quack:review fixture webhook =="
( cd "$gh_mock" && go run ./cmd/qa-mock send \
    --fixture "$fixtures/events/issues.labeled.quack-review.json" \
    --event issues \
    --secret "$secret" \
    --url "${QUACK_WEBHOOK_URL:-http://localhost:8080/api/v1/github/webhook}" )

if [ -z "$chat_id" ]; then
  echo
  echo "No chat id given - listing chats, find the new one and re-run:"
  quack api /api/v1/chats
  exit 0
fi

echo "== polling chat $chat_id =="
for _ in $(seq 1 60); do
  status=$(quack api "/api/v1/chats/$chat_id" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status","?"))')
  echo "status: $status"
  [ "$status" != "running" ] && break
  sleep 5
done

echo "== artifacts =="
quack api "/api/v1/chats/$chat_id/artifacts"

echo "== github-mock deliveries =="
( cd "$gh_mock" && go run ./cmd/qa-mock deliveries --fixtures "$fixtures" )
