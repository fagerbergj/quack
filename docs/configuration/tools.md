# Built-in tools

The `tools:` config section configures quack's builtin tool registry; each agent's `tools:` list in `agents:` binds names from it (ACP agents bind none - they bring their own tools; see [agents.md](agents.md)). `internal/tools/registry.go` is the authoritative list - 13 tools:

| Tool | What it does |
| --- | --- |
| `web_search` | Web search, returns `{results: [{title, url, snippet}]}`. Backend via `tools.web_search.kind`: `exa` (keyless hosted MCP, or REST with `auth.api_key`) or `searxng` (`url` required). |
| `web_fetch` | Read a page. Backend via `tools.web_fetch.kind`: `direct` (plain GET, the default when empty) or `crawl4ai` (GET + browser render, `url` required). |
| `summarize` | Summarize a long text block, optionally focused on a question. |
| `current_date` | The current date, for prompts that need "today". |
| `read_file` | Read a file inside the agent's jail (workspace root + size caps). |
| `list_dir` | List a directory inside the jail. |
| `glob` | Glob file paths inside the jail. |
| `grep` | Search file contents by Go/RE2 regex inside the jail. |
| `check_mermaid` | Validate a mermaid diagram's syntax before it ships in an answer. |
| `stage_memory` | Stage a task memory (store-backed - needs `store:` bound, see [stores.md](stores.md#referencing-a-store-from-a-tool)). |
| `recall_memory` | On-demand recall from the same store `stage_memory` reads (epic #1255 P2) - no `tools:` entry of its own; it takes effect wherever `stage_memory` is bound, plus explicitly in `tools:` lists (see [agents.md](agents.md#recall_memory)). |
| `ask_user` | Pause the node and ask the user a question (answered via `quack chat send` or the UI). |
| `ask_advisor` | Call the advisor agent (reuses the judge's model) mid-run - only wired when the judge is enabled, see [trust-gate.md](trust-gate.md#the-advisor-is-not-a-gate-stage). |

There are deliberately no git or filesystem-write tools in the registry: code agents run as ACP subprocesses with their own edit/shell tools, and delivery (commit/push/PR) is gate-owned (see [trust-gate.md](trust-gate.md#delivery) and [workspace/index.md](workspace/index.md#git_credentials-and-git_push)).

`web_search` and `web_fetch` are the two tools with pluggable backends - `kind` picks the adapter, the rest of the block is that adapter's connection details, the same `kind` shape as [providers and stores](index.md#the-kind-shape).

Two other entries live in `tools:` in `config/quack.yaml` without their own registry constructors: `commit_memory` (the orchestrator's user-memory commit - store binding only, see [agents.md](agents.md#the-orchestrators-tools)) and the memory bindings in general (see [stores.md](stores.md#referencing-a-store-from-a-tool)).
