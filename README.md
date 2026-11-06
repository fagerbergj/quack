# Quack

Quack is a self-hosted agent server for local LLMs. A distributor runs one session process per conversation, each driving an agent bundle on pi-durable, and every sealed artifact can pass an independent judge before it counts. Extensions add the GitHub reviewer and implementer, Sleeper fantasy-football jobs, reMarkable ingest, research, schedules and contracts. A React web UI and the `quack` CLI talk to the distributor over REST and WebSocket.

## Layout

- `packages/` - npm workspaces, TypeScript run directly by Node's type stripping. Core: `sdk` (the extension contract), `session`, `runtime`, `distributor`, `index`, `web`. Extensions: `judge`, `workflows`, `slots`, `traces`, `github`, `home`, `sleeper`, `remarkable`, `research`, `contracts`, `schedules`. Tools: `eval`, `cli`.
- `agents/` - agent bundles (`agent.json`, `prompt.md`, `version.lock`).
- `tools/` - repo gates (`boundary`, `sloplint`, `dupl`, `bundle-locks`, `leak-check`), the QA rig and the reviewer bench.
- `docs/` - the CLI, the QA rig, PR evidence and rollout checks.
- `Dockerfile` - the one image every service runs from.

## Build and test

Needs Node 26.13 (`.node-version`). Everything runs from the repo root.

```bash
npm ci
npm run typecheck
npm test                                  # node:test for packages and tools, then vitest for packages/web
npm run sloplint -- diff origin/<trunk>   # after committing; reads commits only
docker build .
```

Three reMarkable tests need the `rmc` Python module: `pip install --no-deps -r packages/remarkable/requirements.txt`. The Postgres index tests skip unless `QUACK_TEST_DATABASE_URL` is set. The contracts tests need `oasdiff` on `PATH` (the version the Dockerfile pins).

## Run

`node packages/distributor/src/main.ts` serves the API and the web UI on `PORT` (default 8080), reading `QUACK_CONFIG` (the deployment file), `QUACK_AGENTS_DIR` and `QUACK_DATA_DIR`. `node packages/runtime/src/validate.ts` checks a deployment file, the bundles and every session's pinned agent before a rollout; see [docs/README.md](docs/README.md). The CLI is [docs/cli.md](docs/cli.md) and the QA rig is [docs/qa.md](docs/qa.md).

CI (`.github/workflows/v1.yaml`) runs typecheck, tests, sloplint and a live session-isolation test on every pull request, and each trunk push publishes `ghcr.io/fagerbergj/quack:v1-<sha>`.
