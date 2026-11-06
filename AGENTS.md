# AGENTS.md

- Read the untracked `AGENTS.md.local` before labeling a PR `quack:review`, releasing, deploying, restarting llm-swap, or reindexing deepwiki. It carries this host's release and deploy chain and capacity settings. If it is absent, stop and ask; do not reconstruct host-specific commands.
- Work from the repo root: `npm ci`, `npm run typecheck`, `npm test`, and, after committing, `npm run sloplint -- diff origin/<trunk>`. CI runs the same in `.github/workflows/v1.yaml`.
- Functions stay at or under CC 15 and comment runs at most 2 lines; sloplint gates diff-touched lines for both and for duplicated code (`tools/dupl.ts`).
- Packages reach each other only along the layers in `tools/boundary.ts`: extensions reach core through `@quack/sdk`. `tools/boundary.test.ts` enforces it.
- Changing anything in an `agents/<name>/` bundle needs a `version` bump in its `agent.json`, then `node tools/bundle-locks.ts` to rewrite the locks.
- Tests use fake clocks and the injectable timer and fetch seams: no real sleeps, wall clock or network. A flaky test is a defect; fix it or delete it.
- PR evidence by change kind is in `docs/README.md`. A change to the UI or a page route cites a QA rig run (`docs/qa.md`).
- Prefer the `quack` CLI (`docs/cli.md`) over raw REST calls.
