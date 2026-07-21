---
type: Operations Guide
title: Build, Deploy, and CI/CD
description: Quack's build process (frontend-build → embed → compile), test commands, OpenAPI codegen from openapi.yaml via scripts/generate.sh, Docker Compose full-stack deployment (app + Postgres + SearXNG), GitHub Actions CI/CD pipelines, and automated OpenWiki documentation updates.
tags: [build, deploy, ci-cd, docker, operations]
resource: /Makefile
---

# Build, Deploy, and CI/CD

Quack is a Go monorepo with a React 19 frontend SPA that gets embedded into the Go binary at build time. The project uses Make targets for all common operations, Docker Compose for full-stack deployment, and GitHub Actions for CI/CD plus automated OpenWiki documentation updates.

## Build Targets

See also: [Quick Start](/quickstart.md)

### `make build`
Builds the frontend, embeds it into the Go binary, and compiles the server.

1. `make frontend-build` — runs `cd frontend && npm ci && npm run build`, copies `frontend/dist` into `internal/serve/web/dist` (the embed directory), creates a `.gitkeep` placeholder.
2. `go build -o quack ./cmd/quack` — compiles the binary.

### `make generate`
Regenerates Go server stubs and TypeScript client from [`openapi.yaml`](/openapi.yaml) via [`scripts/generate.sh`](/scripts/generate.sh). Uses:
- **oapi-codegen** → generates `internal/schema/quack.gen.go` (Go chi-server stubs + types)
- **@hey-api/openapi-ts** → generates `frontend/src/generated/` (TypeScript client)

After modifying `openapi.yaml`, always run `make generate` and commit the generated files in the same commit. This is enforced as a hard rule in AGENTS.md.

### `make vet` / `make fmt`
- `go vet ./...` — static analysis
- `gofmt -w internal cmd` — format source

### `make test`
Runs `go test ./...` for the full Go test suite.

## Docker Deployment

### `make docker-up` / `make docker-down`
Orchestrates the full stack via `docker compose up --build`. The compose file configures:
- **App** — Quack server binary
- **Postgres** — persistent database for ADK sessions, DAG state, chat metadata
- **SearXNG** — web search engine (JSON API)

### Dockerfile
Multi-stage build: Node.js stage for frontend compilation → Go stage for server compilation → lightweight production image with the embedded binary.

## CI/CD Pipelines

### `.github/workflows/ci.yaml` — Continuous Integration
Runs on push and pull requests:
- `go vet`, `go test ./...`, `gofmt -l` (Go checks)
- `tsc --noEmit`, `eslint src/`, `npm run build`, `npm test` (frontend checks)
- OpenAPI lint (`npx @redocly/cli@latest lint openapi.yaml`)
- Codegen-drift check (`git diff --exit-code -- internal/schema frontend/src/generated`) — ensures generated files match openapi.yaml

### `.github/workflows/cd.yaml` — Continuous Deployment
Handles production/preview deployment of the Quack application.

### `.github/workflows/openwiki-update.yml` — Automated OpenWiki Updates
Scheduled daily at 08:00 UTC (`cron: "0 8 * * *"`) and triggerable manually via `workflow_dispatch`. Uses:
- checkout@v4, Node.js 22 (actions/setup-node@v4)
- Installs OpenWiki globally via npm
- Runs `openwiki code --update --print` with provider `openrouter`, model `z-ai/glm-5.2`
- Creates an update pull request via peter-evans/create-pull-request@v7
- Commit paths: `openwiki/`, `AGENTS.md`, `CLAUDE.md`, `.github/workflows/openwiki-update.yml`

**Environment variables**: Requires `OPENWIKI_PROVIDER=openrouter`, `OPENROUTER_API_KEY`, `OPENWIKI_MODEL_ID=z-ai/glm-5.2`, plus LangSmith tracing (`LANGSMITH_API_KEY`, `LANGCHAIN_PROJECT=openwiki`, `LANGCHAIN_TRACING_V2=true`).

### Dependabot
[`.github/dependabot.yml`](/.github/dependabot.yml) configures automated dependency updates for both Go modules and frontend packages. Recent automated bumps include:
- `google.golang.org/genai` (Go AI SDK)
- `github.com/openai/openai-go/v3` (OpenAI client)
- Vite, Tailwind CSS, Storybook (frontend deps)

## Key Environment Variables

| Variable | Purpose | Default |
|----------|---------|---------|
| `QUACK_DATABASE_URL` | Postgres DSN | Required |
| `QUACK_LLM_ENDPOINT` | OpenAI-compatible LLM endpoint | Required |
| `QUACK_LLM_API_KEY` | API key for LLM endpoint | — |
| `QUACK_ORCH_MODEL` | Orchestrator model name | — |
| `QUACK_RESEARCHER_MODEL` | Researcher/worker model (fallback for coder/media/image) | `QUACK_LLM_ENDPOINT` default |
| `QUACK_CODER_MODEL` | Code agent model | `QUACK_RESEARCHER_MODEL` |
| `QUACK_JUDGE_MODEL` | Judge model (should be different from worker) | `QUACK_LLM_ENDPOINT` default |
| `QUACK_EMBED_MODEL` | Embedding model for vector store | — |
| `QUACK_QDRANT_URL` | qdrant endpoint | Unset = memory self-disables |
| `QUACK_SEARXNG_URL` | SearXNG JSON API endpoint | — |
| `QUACK_WORKSPACE_ROOT` | Filesystem sandbox root | `./workspace` |
| `QUACK_LOG_LEVEL` | slog level: debug, info, warn, error | info |
| `QUACK_LOG_FORMAT` | Output format: text or json | text |
| `QUACK_COMPACTION_ENABLED` | Toggle for history compaction | — |
| `QUACK_COMPACTION_MODEL` | Compaction model | — |

## Generated/Read-Only Files (Never Edit)

Per AGENTS.md hard rules:
- `internal/schema/quack.gen.go` — Go server stubs from OpenAPI codegen
- `frontend/src/generated/` — TypeScript client from OpenAPI codegen
- These are overwritten by `make generate`; hand edits will fail CI.

## Related Concepts

See also: [System Architecture](/architecture/overview.md) · [Quick Start](/quickstart.md)
