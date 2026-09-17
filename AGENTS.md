# AGENTS.md

- Read the untracked `AGENTS.md.local` before running the QA rig, releasing, deploying, restarting llm-swap, or reindexing deepwiki. It carries the rig procedure and its known reds, the release and deploy chain, and this host's capacity settings. If it is absent, stop and ask; do not reconstruct host-specific commands.
- A change reaches `quack:review` only after the QA rig has run on its head; the rig is a single-node regression check, so prove concurrency with race tests instead.
- Comment runs stay at most 2 lines and functions stay at or under CC 15: no narrative block comments, no unbounded branch stacks. CI gates diff-touched code via `tools/sloplint diff` (CC + comment runs) and golangci-lint `dupl` (changed-code duplication).
- A fresh clone builds and boots with nothing vendored: `go build`/`go test` work as-is. Boot seeds and fetches dotagents/ponytail from the plugin registry; offline, the embedded snapshot serves dotagents' skills only, and ponytail's are absent until a fetch succeeds.
- Run `npm --prefix scripts ci` before vetting tests; otherwise Mermaid validation coverage can fail or be skipped for missing dependencies.
- After editing `openapi.yaml`, run `make generate` and commit changes under `internal/schema/` and `frontend/src/generated/`. Never hand-edit those generated files.
- Prefer the `quack` CLI over raw REST calls; use `docs/cli.md` as the command map.
- Follow `docs/configuration/agents.md` when changing an agent bundle. No files beyond `agent-card.json`, `prompt.md`, optional `rubric.yaml`, and optional `memory.md` belong in a bundle.
- Before changing `internal/workspace/` sandboxing or ACP env/argv wrapping, run `quack sandbox check --agent code-reviewer`; see `docs/sandbox-cli.md`.
- Exercise user-facing features on a non-production server and state what UI or CLI path was checked in the PR.
