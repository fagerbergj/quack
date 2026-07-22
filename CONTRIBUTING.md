# Contributing to quack

Thanks for hacking on quack. This is the contribution **workflow**; for the deep architecture and the project's hard rules, read [AGENTS.md](AGENTS.md) (the agent / developer guide — `CLAUDE.md` is a symlinked copy).

## The working model

quack is built the way quack works — **issue-driven and agent-executed**. Every non-trivial change follows the same loop, whether a human or quack does the work. The `quack:plan` → `quack:implement` → `quack:review` → `quack:merge` label workflow is this loop automated (see [docs/extensions/github.md](docs/extensions/github.md)).

1. **File an issue.** State a concrete failure (not a vague wish), what's **in and out of scope**, **forbidden actions**, and **acceptance criteria**. Confirm it's real and not already done. Label it: type (`bug` / `feature-request` / `enhancement`), `area:*`, and `priority:*`.
2. **Plan.** Post an implementation plan as a comment — **grounded in the code** (real `file:line` anchors), **summary-first**, a `mermaid` diagram where structure helps, deep detail in `<details>`. Or apply **`quack:plan`** to have quack plan it on the issue's session.
3. **Agree.** Refine the plan in the thread until it holds — **reuse or extend before you add**, correct scoping, honest constraints. Do not implement before the plan is settled.
4. **Implement.** Apply **`quack:implement`** (quack implements on the same session and opens a PR pre-labeled for review) or implement on a branch. Reuse before adding; write the failing test first, implement to green.
5. **Review.** The PR is reviewed (**`quack:review`** or a human). **Verify every finding** — reject false positives *with a reason*, fix the real ones.
6. **Merge.** **`quack:merge`** does a human-authorized, quack-approved squash merge; or combine related PRs into one branch and merge with green CI.

The per-step craft lives in loadable skills — `plan-work`, `develop-feature`, `fix-bug`, `review-code`, and `contribute` (which points back here).

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
- If you change **`openapi.yaml`**, run `make generate` and commit the regenerated files in the same PR — CI has a codegen-drift check that fails otherwise.

## CI / CD

- **CI** (`.github/workflows/ci.yaml`) runs on every PR and push: `go vet`, `go test`, `gofmt -l`, `tsc --noEmit`, `eslint`, `npm run build`, `npm test`, OpenAPI lint, codegen-drift, and a docker build. **All must pass** — `main` is branch-protected.
- **CD** (`.github/workflows/cd.yaml`) publishes the image `ghcr.io/fagerbergj/quack`:
  - **Every merge to `main`** moves **`:latest`** (+ `sha-<sha>`). The home-server deployment (`home-server/quack/`) runs `:latest` and **Watchtower auto-deploys** each merge.
  - **A version tag** publishes an **immutable pinned snapshot** — cut one with `git tag vX.Y.Z && git push origin vX.Y.Z` → `X.Y.Z`, `X.Y`, `sha-<sha>`.
  - `:latest` tracks `main`, **not** tags, so cutting a pinned release never silently moves prod.

  In short: **merge = continuous deploy; tag = a marked release.**

## Commits & PRs

- **Conventional commits** — `feat(scope): …`, `fix(scope): …`, `docs(scope): …`, `ci(scope): …`.
- **Comments say what the code CANNOT** (non-obvious constraints, invariants, the ceiling of a deliberate shortcut). The incident/story goes in the commit message and PR body, not the source (AGENTS.md "Comments").
- **Non-trivial features are spec-first** (AGENTS.md "Spec-Driven Development"): scope, forbidden actions, available interfaces, output contract, and 2–3 concrete test cases in the PR description. Behavioural drift from the spec becomes a failing test, not a production incident.

## Adding or changing an agent

An agent is a **bundle** under `agents/<name>/` — exactly `agent-card.json` + `prompt.md` (plus the optional `rubric.md` and `memory.md`). `config/quack.yaml` binds it to a model and a tool list. **No Go changes are needed** to add or modify an agent (AGENTS.md "Agent bundles").
