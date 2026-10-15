# AGENTS.md

- Read the untracked `AGENTS.md.local` before labeling a PR `quack:review`, running the QA rig, releasing, deploying, restarting llm-swap, or reindexing deepwiki. It carries the evidence each kind of change needs before review, the rig procedure and its known reds, the release and deploy chain, and this host's capacity settings. If it is absent, stop and ask; do not reconstruct host-specific commands.
- Prompts, rubrics, skills and tool definitions stay within `tools/promptlint/promptlint.yaml` budgets and allowlists; CI gates diff-touched items with `tools/promptlint diff`, see `docs/promptlint.md`.
- Comment runs stay at most 2 lines and functions stay at or under CC 15: no narrative block comments, no unbounded branch stacks. CI gates diff-touched code via `tools/sloplint diff` (CC + comment runs) and golangci-lint `dupl` (changed-code duplication).
- A fresh clone builds and boots with nothing vendored: `go build`/`go test` work as-is. Boot seeds and fetches dotagents, ponytail, sleeper, github, usage and remarkable from the plugin registry; offline, the embedded snapshot serves dotagents' skills only, and ponytail's skills, sleeper's skills, agents and workflows, github's code agents and remarkable's document-ingest are absent until a fetch succeeds.
- Extensions under `extensions/` reach core only through `extensions/sdk`, a separate module that `go test ./...` skips (`make test` covers it); `extensions/boundary_test.go` enforces the boundary. Files under `extensions/*/plugin` ship to deployments only through a `plugin/<name>/vX.Y.Z` tag and a seed bump, never the image; see `docs/extensions/dev-loop.md`.
- Run `npm --prefix scripts ci` before vetting tests; otherwise Mermaid validation coverage can fail or be skipped for missing dependencies.
- After editing `openapi.yaml`, run `make generate` and commit changes under `internal/schema/` and `frontend/src/generated/`. Never hand-edit those generated files.
- Prefer the `quack` CLI over raw REST calls; use `docs/cli.md` as the command map.
- Follow `docs/configuration/agents.md` when changing an agent bundle. No files beyond `agent-card.json`, `prompt.md`, optional `rubric.yaml`, and optional `memory.md` belong in a bundle.
- Before changing `internal/workspace/` sandboxing or ACP env/argv wrapping, run `quack sandbox check --agent code-reviewer`; see `docs/sandbox-cli.md`.
- Exercise user-facing features on a non-production server and state what UI or CLI path was checked in the PR.
