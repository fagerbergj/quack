## What to remember

This is the quack backend: a Go module (`github.com/fagerbergj/quack`) whose single source of truth is
`openapi.yaml`. Any change to it requires running `make generate` and committing the regenerated files
(`internal/schema/quack.gen.go`, `frontend/src/generated/`). **Never edit those generated files directly.**

The internal layout is:

- `internal/server/` — chi router (`router.go`) + generated strict REST handlers in `rest/`
- `internal/orchestrator/` — the orchestrator loop; delegates to DAG planner + native ADK graph
- `internal/dag/` — plan decomposition, execution (nativegraph), and SSE event streaming (executor)
- `internal/vetting/` — trust gate: worker rounds, judge scoring, revisions, delivery checks
- `internal/tools/` — builtin agent tools (ask_advisor is here)
- `internal/inference/` — single LLM factory (`inference.NewModel`) for `model.LLM`
- `internal/acp/` — Agent Client Protocol transport for external subprocess agents
- `internal/memory/` — memory store abstraction and scope resolution
- `internal/workspace/` — filesystem jail (sandbox) with `NewJail` + capabilities

Error handling: log once at the handling boundary (`slog`), never log-and-return. `component` attribute
per subsystem, per-instance `*slog.Logger` field for stable objects. Test files live beside source files;
use table-driven tests with `t.Run` subtests and `t.Parallel`. The CI matrix checks: `go vet`, `go test ./...`, `gofmt -l`, TypeScript + ESLint, build, frontend Vitest.

When working on a feature, run the full gate (`make generate`, `go test ./...`, `make vet && make fmt`)
before claiming it passes — the review gate re-runs your claims.
