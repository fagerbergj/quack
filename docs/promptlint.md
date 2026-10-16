# promptlint

`tools/promptlint` measures every prompt, rubric, skill and tool definition quack sends a model, and fails on three kinds of defect: text over its token budget, a reference that does not resolve, and two overlapping tools offered to the same agent. Budgets and allowlists live in `tools/promptlint/promptlint.yaml`.

```sh
make prompts                                    # full check: report plus every finding gated
go run ./tools/promptlint diff origin/main      # CI's mode: gate only what the diff touches
go run ./tools/promptlint -json report.json check   # also write the machine-readable report
```

## What it measures

Tokens are estimated as bytes/4 (no tokenizer is a dependency); treat the numbers as relative sizes, not billing.

- **Files**: each agent bundle's `prompt.md`, `memory.md` and `rubric.yaml` (under `agents/` and `extensions/*/plugin/agents/`), `config/prompts/*.md`, `config/constitution.md`, `config/rubric.md`, and every `SKILL.md` under `skills/`, `extensions/*/plugin/skills/` and the embedded dotagents snapshot.
- **Tools**: the real declarations, built with stub dependencies from the builtin registry, the per-dispatch artifact tools, the orchestrator's plan and memory tools, ADK's skill toolset, and every registered extension's `Tools()`. A tool's size is the JSON a provider receives: name, description and parameter schema.
- **Agents**: system prompt (`prompt.md` plus `memory.md`) + every tool the agent is offered + the skill list (each listed skill's `SKILL.md` frontmatter). The offered set follows serve's own rules: `tools.ResolveNames`, the always-on `list_artifacts`/`read_artifact`, the write tools a card's `artifact` implies, the skill toolset when `skills:` is set. Plugin `agent.yaml` fields are overridden by `config/quack.yaml` the way plugin seeding does. The orchestrator's fixed tools are listed in the config file, mirroring `internal/orchestrator/run.go`.

Not measured yet: ACP agents' loopback MCP tools (`internal/acp/memorymcp.go`), the judge's own tool set, and the skill toolset's fixed system instruction.

## What it fails

| Rule | Fails when |
| --- | --- |
| `budget` | an item or agent total exceeds its kind's budget or its per-item override |
| `unknown_tool` | an agent is offered a tool no builtin, core tool or extension declares |
| `unknown_skill` | a `skills:` entry or a `load_skill("x")` in a prompt resolves under no skill dir (a plugin listed in `external_skills` only warns) |
| `missing_link` | a relative Markdown link in a prompt or skill points at no file (links in code spans and fences are examples) |
| `missing_resource` | a `references/`, `assets/` or `scripts/` path in a `SKILL.md`, or a prompt's `` `plugin:skill`'s `path` ``, does not exist |
| `rubric_invalid` | a `rubric.yaml` fails the loader serve uses (`vetting.LoadReplayRubric`) |
| `rubric_undefined_criterion` | a criterion is `deterministic: true` but code computes no criterion of that name (`vetting.CodeOwnedCriteria`), so it is never scored |
| `overlap` | two tools whose similarity reaches `overlap.threshold` are offered to one agent and no `allow` entry covers the pair |

Similarity is the Jaccard index of the two tools' word sets (name plus description, lowercased, stopwords dropped, plurals and tenses folded). The report lists every pair above the threshold, including ones no agent is offered together, so a near-duplicate stays visible before anyone wires both into one agent.

## Changing the config

Every `allow` entry and `ignore` entry needs a one-line `reason`; the loader rejects one without. An `allow` entry that no longer covers any co-offered pair is reported as `stale_allowlist` so the list shrinks with the code. Overrides were seeded at each item's size plus about 10%: shrink one when its item shrinks, and justify any raise in the PR. In diff mode a finding gates when the diff touches one of its sources (the file, the Go tree that declares the tool, the agent's bundle and `config/quack.yaml`); editing `tools/promptlint/promptlint.yaml` regates everything.
