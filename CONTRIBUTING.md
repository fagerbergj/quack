# Contributing to quack

Thanks for hacking on quack. This is the contribution **workflow**; for the deep
architecture and the project's hard rules, read [AGENTS.md](AGENTS.md) (the agent /
developer guide — `CLAUDE.md` is a symlinked copy).

## Development setup

- **Go** — module `github.com/fagerbergj/quack`; server entrypoint `cmd/server/main.go`.
- **Frontend** — `cd frontend && npm install`, then `npm run dev` (hot reload on :5173).
- **Full build** — `make build` (compiles the frontend and embeds `dist` into the binary).
- **Local stack** — `make docker-up` brings up app + Postgres + searxng + qdrant via Docker.

## Before you open a PR

Run what CI enforces (see AGENTS.md "Hard Rules" for the full never/always list):

```bash
go test ./...                 # add -race for concurrency changes
make vet && make fmt
cd frontend && npm test && npx tsc --noEmit && npx eslint src/
```

- **Never** hand-edit generated files (`internal/schema/quack.gen.go`, `frontend/src/generated/`).
- If you change **`openapi.yaml`**, run `make generate` and commit the regenerated files in the
  same PR — CI has a codegen-drift check that fails otherwise.

## CI / CD

- **CI** (`.github/workflows/ci.yaml`) runs on every PR and push: `go vet`, `go test`, `gofmt -l`,
  `tsc --noEmit`, `eslint`, `npm run build`, `npm test`, OpenAPI lint, codegen-drift, and a docker
  build. **All must pass** — `main` is branch-protected.
- **CD** (`.github/workflows/cd.yaml`) publishes the image `ghcr.io/fagerbergj/quack`:
  - **Every merge to `main`** moves **`:latest`** (+ `sha-<sha>`). The home-server deployment
    (`home-server/quack/`) runs `:latest` and **Watchtower auto-deploys** each merge.
  - **A version tag** publishes an **immutable pinned snapshot** — cut one with
    `git tag vX.Y.Z && git push origin vX.Y.Z` → `X.Y.Z`, `X.Y`, `sha-<sha>`.
  - `:latest` tracks `main`, **not** tags, so cutting a pinned release never silently moves prod.

  In short: **merge = continuous deploy; tag = a marked release.**

## Commits & PRs

- **Conventional commits** — `feat(scope): …`, `fix(scope): …`, `docs(scope): …`, `ci(scope): …`.
- **Comments say what the code CANNOT** (non-obvious constraints, invariants, the ceiling of a
  deliberate shortcut). The incident/story goes in the commit message and PR body, not the source
  (AGENTS.md "Comments").
- **Non-trivial features are spec-first** (AGENTS.md "Spec-Driven Development"): scope, forbidden
  actions, available interfaces, output contract, and 2–3 concrete test cases in the PR description.
  Behavioural drift from the spec becomes a failing test, not a production incident.

## Adding or changing an agent

An agent is a **bundle** under `agents/<name>/` — exactly `agent-card.json` + `prompt.md` (plus the
optional `rubric.md` and `memory.md`). `config/quack.yaml` binds it to a model and a tool list. **No
Go changes are needed** to add or modify an agent (AGENTS.md "Agent bundles").
