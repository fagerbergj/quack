---
name: contribute
description: >
  The issue-driven, agent-executed working model for contributing to quack:
  file an issue → post a plan → agree → implement → review → merge. Load this
  before starting ANY contribution to the quack repo — before planning or writing
  code. It points to CONTRIBUTING.md (the authoritative version) and hands off to
  the per-step skills (plan-work, develop-feature, fix-bug, review-code).
---

# Contribute to quack: issue → plan → agree → implement → review → merge

quack is built the way quack works — **issue-driven and agent-executed**. The
authoritative version of this model lives in `CONTRIBUTING.md` at the repo root;
**read it first**. This skill is the loadable summary and the pointers to the
per-step craft.

The `quack:plan` → `quack:implement` → `quack:review` → `quack:merge` label
workflow is this loop automated.

## The loop

1. **File an issue.** A concrete failure, not a wish. State scope (in/out),
   forbidden actions, and acceptance criteria. Confirm it's real and not already
   done. Label it: type (`bug`/`feature-request`/`enhancement`), `area:*`,
   `priority:*`.
2. **Plan.** Post an implementation plan as an issue comment — grounded in the
   code (`file:line` anchors), summary-first, a `mermaid` diagram where structure
   helps, deep detail folded in `<details>`. For DAG-shaped work, load
   **`plan-work`**.
3. **Agree.** Refine in-thread until the plan holds. Reuse or extend before you
   add (see **`develop-feature`** step 2). Don't write code before it's settled.
4. **Implement.** Load **`develop-feature`** (new capability) or **`fix-bug`** (a
   defect). Reuse before adding; write the failing test first, implement to green.
5. **Review.** Load **`review-code`**. Verify every finding — reject false
   positives *with a reason*, fix the real ones.
6. **Merge.** Human-authorized, quack-approved squash merge (or combine related
   PRs into one branch), green CI required.

## Hard rules (from AGENTS.md — non-negotiable)

- **Never** hand-edit generated files (`internal/schema/quack.gen.go`,
  `frontend/src/generated/`). Run `make generate` after any `openapi.yaml` change
  and commit the regenerated output in the same PR (CI has a codegen-drift check).
- `go test ./...` and `cd frontend && npm test` must pass; `make vet && make fmt`
  before committing Go changes.
- Non-trivial features are **spec-first**: scope, forbidden actions, interfaces,
  output contract, and 2–3 concrete test cases. Behavioural drift from the spec
  becomes a failing test, not a production incident.

See `CONTRIBUTING.md` for the full detail and `AGENTS.md` for the architecture.
