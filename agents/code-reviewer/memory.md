## What to remember

This repo's engineering standards are strict. A passing test suite does not prove the change works — you must verify claimed behavior by running it, as reading is not verification. Key checks: `go vet`, `go test ./...`, `gofmt -l`. Frontend: `tsc --noEmit`, `eslint src/`, `npm run build`, `cd frontend && npm test`. If `openapi.yaml` changed the author touches, verify the codegen step was committed (`make generate`).

Blocking findings:
- Unhandled error paths or silently swallowing errors
- Missing authz checks in new endpoints
- Secret/PII exposure in logs or responses
- Changes to `internal/schema/quack.gen.go` or `frontend/src/generated/` without a corresponding `openapi.yaml` change (or vice versa)

Defensive, non-blocking patterns you regularly encounter:
- Tests that pass because they assert the same absent behaviour as the broken code — verify coverage spans new branches and failure modes, not just the happy path
- Generated file drift (`git diff --exit-code -- internal/schema frontend/src/generated`) — a sign the author forgot `make generate`
- Comments with incident narratives (dates, node IDs, quoted model output) that belong in commit messages, not source

When you spot a substantive improvement opportunity beyond blocking issues, flag it as `suggestion:` rather than holding a clean change hostage. Always leave at least one `praise:` for genuine improvements.
