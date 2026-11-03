# Contracts extension

Status: draft, 2026-11-03. Issue #2065. A feature extension (core = harness plus generic hooks, D-decisions); the judge is optional (#2060).

**Job.** Someone describes what a contract must do and for whom. The contracts agent reads the code around it, asks the open questions one at a time, and seals a versioned `contract.spec` in which every choice links to why. A change request seals the next version with its changes and a compatibility note that names affected callers.

## Pieces

- **Kind plugin** (`ContractKind`): `extract` (a contract file to its standard document), `check` (mechanical problems), `diff` (breaking changes between versions), `renderHints`, and `skill` (the authoring guide, also the rubric source). Only `rest` exists.
- **REST checks**: Spectral with the maintained `oas` ruleset plus the guide's checks as declarative rules (`rest-rules.ts`). Breaking changes come from oasdiff (pinned in `v1/Dockerfile` and CI; `QUACK_OASDIFF` names the binary). Neither is hand-rolled.
- **Questions**: the `ask_question` tool runs the SDK's `decide` with the `person` resolver. Options are the suggested answer first, the alternatives, then "You decide"; the evidence holds why it matters and each trade-off.
- **Judge** (`submit_contract`): code gates for a clean document, the version number, resolving links (answered questions by their text, guide anchors, `path:line` in the workspace) and breaking changes with named callers; model checks for intent versus spec, guide agreement and unasked additions.
- **Approval** (`propose_contract`): a gated tool the agent calls when the person has no more changes. The approval carries the version, the open `check` findings and the breaks against the last approved version. A decision is recorded as `contract.approved` or `contract.changes` (outcome `vN`), written idempotently from the stored verdict. An approved version is not proposed again.
- **Contracts page** (web nav): every `contracts:*` session with its latest version and status (Drafting, Awaiting your approval, Approved vN, Changes requested), plus "New contract".
- **Action** `define_contract` starts a `contracts:<id>` session so Home can offer "define a contract for ...".

## Spec version numbers

`version` in the spec is one more than the number of delivered (passed or released) earlier specs. A revision of a failing attempt is a new sealed artifact version but the same spec version.

## Not yet

Kinds other than REST; free-text answers (the questions mechanism accepts only a listed option); checking that a named caller exists.
