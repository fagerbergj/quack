# AGENTS.md

- Read the untracked `AGENTS.md.local` before deploying, restarting llm-swap, or reindexing deepwiki. If it is absent, stop and ask; do not reconstruct host-specific commands.
- On a fresh clone, run `make plugins`. Use `make test`, not bare `go test ./...`, so the untracked embedded skill trees are present.
- Run `npm --prefix scripts ci` before vetting tests; otherwise Mermaid validation coverage can fail or be skipped for missing dependencies.
- After editing `openapi.yaml`, run `make generate` and commit changes under `internal/schema/` and `frontend/src/generated/`. Never hand-edit those generated files.
- Prefer the `quack` CLI over raw REST calls; use `docs/cli.md` as the command map.
- Follow `docs/configuration/agents.md` when changing an agent bundle. No files beyond `agent-card.json`, `prompt.md`, optional `rubric.yaml`, and optional `memory.md` belong in a bundle.
- Before changing `internal/workspace/` sandboxing or ACP env/argv wrapping, run `quack sandbox check --agent code-reviewer`; see `docs/sandbox-cli.md`.
- Exercise user-facing features on a non-production server and state what UI or CLI path was checked in the PR.
