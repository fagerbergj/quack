---
type: Quick Start Guide
title: Quack Wiki - Quick Start
description: Introduction to the Quack knowledge base - what Quack is, how to run it, and navigation to all documented sections covering architecture, workflows, domain concepts, operations, and integrations.
tags: [overview, quickstart]
resource: /README.md
---

# Quack Wiki - Quick Start

## What Is Quack?

Quack is a local LLM assistant that handles tasks through adversarial multi-agent collaboration. Instead of trusting a single model's output, each agent's work passes through a **trust gate** - a generate→critique→revise→judge loop where an independent judge model scores results against acceptance criteria before they propagate downstream or reach the user.

The name comes from its design: it's the rubber duck that *talks back* (it answers), and whose agents *talk back to each other* (adversarial vetting).

## Key Concepts

- **Tasking** - Clients (Opencode, Claude Code, web SPA) submit natural-language requests through a REST/MCP gateway.
- **DAG Planning** - An orchestrator decomposes the request into a directed acyclic graph of agent nodes.
- **Adversarial Vetting** - Every node's output is verified by an independent judge model before downstream use.
- **Memory** - Only adversarially-vetted findings are committed to semantic memory (qdrant).

## How to Run

```bash
# Full stack via Docker Compose (app + Postgres + SearXNG)
make docker-up

# Local build and run
QUACK_DATABASE_URL=... QUACK_LLM_ENDPOINT=... QUACK_ORCH_MODEL=... make run

# Frontend dev server
cd frontend && npm run dev
```

## Configuration

Key environment variables:

| Variable | Purpose |
|----------|---------|
| `QUACK_LLM_ENDPOINT` | OpenAI-compatible LLM endpoint |
| `QUACK_LLM_API_KEY` | API key for the LLM provider |
| `QUACK_ORCH_MODEL`, `QUACK_RESEARCHER_MODEL`, `QUACK_CODER_MODEL`, `QUACK_JUDGE_MODEL` | Per-role model names |
| `QUACK_DATABASE_URL` | Postgres DSN |
| `QUACK_QDRANT_URL` | qdrant endpoint for semantic memory |
| `QUACK_SEARXNG_URL` | SearXNG web search endpoint |
| `QUACK_WORKSPACE_ROOT` | Filesystem sandbox root (default `./workspace`) |

See [`docs/configuration/index.md`](/docs/configuration/index.md) for full configuration details.

## Documentation Structure

This wiki covers Quack across several areas:

- **[System Architecture](/architecture/overview.md)** - monorepo layout, request lifecycle, API contract via OpenAPI codegen, native vs ACP agents, streaming vocabulary, model factory, data stores
- **[Adversarial Trust Gate](/architecture/vetting.md)** - the generate→critique→revise→judge loop, judge independence, deterministic checks, plan judge with isolated repo clone
- **[Workspace Isolation](/architecture/workspace-jail.md)** - per-user path scoping, jail containment rules, repo provisioning via `SetupClone` tools, shared vs plan-judge scopes
- **[DAG Execution](/workflows/dag-execution.md)** - DAG planning and validation by the orchestrator, plan judge scoring, topological node execution via ADK workflow graphs, delivery mechanics
- **[Build & Deploy](/operations/deployment.md)** - Makefile targets, OpenAPI codegen pipeline, Docker Compose stack, CI checks, GitHub Actions

## Recent Changes

- **OpenWiki workflow updates (commit a0bcf48 → HEAD)** - The automated OpenWiki update workflow (`openwiki code --update --print`) runs daily at 08:00 UTC via `cron: "0 8 * * *"`. Auth provider is `openrouter` with model `z-ai/glm-5.2`. It uses Node.js 22, actions v4 (checkout + setup-node), and un-pinned global npm install. add-paths include `openwiki`, `AGENTS.md`, `CLAUDE.md`, and the workflow file itself.

## Backlog

The following areas were identified for future documentation updates:

- **Login flow details** - recent refactor (commit 05a67a3, PR #524) replaced device grant login with auth code + PKCE; see [`docs/cli.md`](/docs/cli.md), [`docs/configuration/auth.md`](/docs/configuration/auth.md), and [`internal/cli/login.go`](/internal/cli/login.go)
- **Per-user memory scoping** - recent change (commit c2ec866, PR #512) threads commenter login for per-user scoping in memory; see [`internal/store/store.go`](/internal/store/store.go) and [`internal/github/webhook.go`](/internal/github/webhook.go)
- **ACP protocol details** - agent-client-subprocess communication via `internal/acp/translate.go` and the Agent Client Protocol
- **Git tools deep dive** - detailed look at `git_clone`, `git_commit`, and `PushBranch` tool implementations in `internal/tools/`
- **Frontend component architecture** - React components under `frontend/src/components/` and state management details
