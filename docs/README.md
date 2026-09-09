# Documentation

Setup and configuration guides for humans to use quack. For how the code itself is put together, see [`AGENTS.md`](../AGENTS.md).

- [`cli.md`](cli.md) - the CLI: the main command tree, `-p`, exit codes (recording/replay/eval/validate have no prose yet).
- [`ui.md`](ui.md) - the web SPA: what it does, running it, contributing to it.
- [`api.md`](api.md) - the REST/MCP/A2A surface and streaming.
- [`configuration/`](configuration/) - models, agents, the trust gate, stores, auth, workspace sandboxing, deployment shapes, observability.
- [`agent-plugins.md`](agent-plugins.md) - the Agent Plugins standard: how a plugin ships skills, MCP servers, and quack extension declarations.
- [`extensions/github.md`](extensions/github.md) - the GitHub App.
- [`extensions/ui-kit.md`](extensions/ui-kit.md) - the CSS kit an extension's own HTML pages link to look native to the SPA.
- [`sandbox-cli.md`](sandbox-cli.md) - `quack sandbox`: entering or probing the real agent jail.
- [`memory-lifecycle.md`](memory-lifecycle.md) - the memory lifecycle design: how a memory is committed, recalled, and forgotten.
- [`qa-mocks.md`](qa-mocks.md) - exercising the GitHub and reMarkable flows against a QA server, no real credentials.
- [`frontend-testing.md`](frontend-testing.md) - the frontend test setup: Vitest, render-checks, Storybook.
- [`faq.md`](faq.md) - the *why* behind a few choices that look surprising from outside.

For the contribution workflow (issue → plan → implement → review → merge) see [`../CONTRIBUTING.md`](../CONTRIBUTING.md); for the agent operating rules see [`../AGENTS.md`](../AGENTS.md).
