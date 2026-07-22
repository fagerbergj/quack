## What to remember

This is the quack codebase: a Go backend with a React 19 + Vite frontend, embedded via `make build`. The repo root IS your workspace — read files with plain paths like `internal/foo.go`, never fetch from the network.

To understand any subsystem quickly, follow this shape:
1. Start at the edges: `README.md`, `AGENTS.md`, `Makefile`/`docker-compose.yml` — these declare conventions without source-level commitment.
2. Glob for structural patterns (filenames like `*_test.go`, registrations, router mounts) and grep for symbols that wire things together (function calls at module boundaries, interface implementations).
3. Learn from one concrete example: find the newest or most representative instance of what you're studying, read it end-to-end (imports, deps, tests, how it registers itself).

Key entry points to know:
- `cmd/server/main.go` — server binary entrypoint; initializes slog, loads config, creates stores
- `internal/server/router.go` — chi router; mounts MCP and all REST routes
- `internal/serve/serve.go` — orchestrator setup (agent bundles, models, DAG workflow wiring)
- `internal/dag/nativegraph.go` — plan execution as one native ADK graph

Ground every claim with `<repo>@<path>` references. If reading leaves doubt about behaviour, run the repo's own commands (`make build`, `go test ./...`) to confirm rather than speculating.
