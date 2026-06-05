#!/usr/bin/env bash
# Regenerate all code from the OpenAPI spec (single source of truth):
#   - the Go chi-server + models (oapi-codegen)
#   - the TypeScript client (openapi-ts)
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> Go server types (oapi-codegen)"
go generate ./internal/schema/...

echo "==> TypeScript client (openapi-ts)"
( cd frontend && npm run generate )

echo "Done."
