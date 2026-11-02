# Quack v1 design

The rewrite's requirements and design, in the order they are signed off. No code is written until all of them are. All three are signed off as of 2026-10-30.

1. **Requirements** (signed off 2026-10-30): [PRD](prd.md) (why and what), [SRS](srs.md) (core requirements), and one short document per extension in [extensions/](extensions/). They trace to [decisions.md](decisions.md), the log of settled answers: decisions (`D*` and the per-extension prefixes), architecture entries (`A*`), or the operator's platform requirements (`P`), which each such SRS row restates in full.
2. **Architecture** (signed off 2026-10-30): [RFC 0001](rfc/0001-core.md) for core and one RFC per extension in [rfc/extensions/](rfc/extensions/), built on the layers in [decisions A3](decisions.md).
3. **UI mockups** (signed off 2026-10-30): [mockups/whimsical.html](mockups/whimsical.html) is the approved design (published at https://claude.ai/artifact/NR45F1ypJX1WrpaQKbBwLy). [mockups/index.html](mockups/index.html) is the plainer v3 it grew from. Design rules: sessions workspace as home; mallard palette; whimsy in visuals, motion and helper notes only, never in control labels or control icons.

## Evidence for v1 pull requests

Until v1 runs somewhere it can be rigged, a v1 pull request into the `v1` trunk shows green v1 CI (typecheck, tests, sloplint) and, for any change on the model path, a 1-3 item smoke test against local llm-swap, with the commands and output in the PR. v0's QA rig in `AGENTS.md.local` covers v0 only.

## Checking a rollout

Before `compose up` with a new image, run its validator against the prod config, agents and data volume, mounted read-only. It loads every bundle and every session's pinned `agent-def/agent.json` with the deployment-level extensions (unset extension env gets placeholders, nothing is contacted), prints `FAIL <path>: <error>` per failure and `N loaded, M failed`, and exits non-zero on any failure:

```
docker run --rm -v <data>:/data:ro -v <config>:/config/deployment.json:ro -e QUACK_CONFIG=/config/deployment.json -e QUACK_AGENTS_DIR=/app/agents -e QUACK_DATA_DIR=/data <new-image> node packages/runtime/src/validate.ts
```
