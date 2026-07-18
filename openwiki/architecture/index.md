---
type: Documentation Index
title: "Architecture"
description: "Files and subdirectories in Architecture."
---

# Files

- [Quack System Architecture](overview.md) - High-level system architecture of Quack — monorepo layout, request lifecycle through the HTTP gateway to DAG execution and adversarial vetting, OpenAPI-driven code generation, native vs ACP agent types, SSE streaming vocabulary, model factory, and data stores (Postgres + qdrant).
- [Adversarial Trust Gate](vetting.md) - The trust gate that every DAG node's output must pass before propagating downstream — self-refine loop, deterministic checks (citations, length, build/vet/test), and independent judge scoring. Covers RunGatedRefine, PlanJudgeScope isolation, setupCloneReadOnly for the plan judge, fail-closed on budget errors, and deterministic-check skip signals via OTel metrics.
- [Workspace Isolation (Jail)](workspace-jail.md) - The workspace jail — a per-user filesystem containment boundary that every file and git tool resolves through. Covers Jail struct, path resolution rules (userID/chatID scoping, ErrEscape/ErrInvalidUserID errors), the shared vs plan-judge reserved scopes, HomeDir fix, and NodeDir per-node isolation.
