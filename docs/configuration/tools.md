# Built-in tools

The `tools:` config section configures quack's builtin tool registry; each agent's `tools:` list in `agents:` binds names from it (ACP agents bind none - they bring their own tools; see [agents.md](agents.md)). `internal/tools/registry.go` is the authoritative list - 14 tools:

| Tool | What it does |
| --- | --- |
| `web_search` | Batched web search: `queries: [...]` (up to 20), one group of `{title, url, snippet}` per query, deduplicated by URL across the batch. Backend via `tools.web_search.kind`: `exa` (keyless hosted MCP, or REST with `auth.api_key`) or `searxng` (`url` required). |
| `web_fetch` | Batched page fetch: `urls: [...]` (up to 20), fetched concurrently, one result per URL and a per-URL failure that doesn't fail the batch. A page under the inline threshold (24 KB) comes back in full; at or above it, or once the call's combined inline text passes a ~60 KB budget, the page is stored as a `web_page` artifact of the chat and the entry is a short header - see `grep_artifacts` below. `pattern`/`offset` still shape the full page directly as a shortcut. Backend via `tools.web_fetch.kind`: `direct` (plain GET, the default when empty) or `crawl4ai` (GET + browser render, `url` required). |
| `grep_artifacts` | Regex search across the chat's stored `web_page` artifacts (or a given `ids` subset), returning `artifact:line: text` hits - pairs with `read_artifact`'s `offset`/`lines` window to read around a hit without re-fetching. |
| `summarize` | Summarize a long text block, optionally focused on a question. |
| `current_date` | The current date, for prompts that need "today". |
| `read_file` | Read a file inside the agent's jail (workspace root + size caps). |
| `list_dir` | List a directory inside the jail. |
| `glob` | Glob file paths inside the jail. |
| `grep` | Search file contents by Go/RE2 regex inside the jail. |
| `check_mermaid` | Validate a mermaid diagram's syntax before it ships in an answer. |
| `stage_memory` | Stage a task memory (store-backed - needs `store:` bound, see [stores.md](stores.md#referencing-a-store-from-a-tool)). |
| `recall_memory` | On-demand recall from the same store `stage_memory` reads (epic #1255 P2) - no `tools:` entry of its own; it takes effect wherever `stage_memory` is bound, plus explicitly in `tools:` lists (see [agents.md](agents.md#recall_memory)). |
| `load_memory` | Same implementation as `recall_memory`, registered under this name too - listing both in one `tools:` collapses to whichever is listed first (see [agents.md](agents.md#recall_memory)). |
| `ask_user` | Pause the node and ask the user a question (answered via `quack chat send` or the UI). |

There are deliberately no git or filesystem-write tools in the registry: code agents run as ACP subprocesses with their own edit/shell tools, and delivery (commit/push/PR) is gate-owned (see [trust-gate.md](trust-gate.md#delivery) and [workspace/index.md](workspace/index.md#git_credentials)).

`web_search` and `web_fetch` are the two tools with pluggable backends - `kind` picks the adapter, the rest of the block is that adapter's connection details, the same `kind` shape as [providers and stores](index.md#the-kind-shape).

One other entry lives in `tools:` in `config/quack.yaml` without its own registry constructor: `commit_memory` (the orchestrator's user-memory commit - store binding only, see [agents.md](agents.md#the-orchestrators-tools)).

## Skills

Skills are not a `tools:` entry - they come from the dynamic plugin registry, configured under the top-level `plugins:` key:

```yaml
plugins:
  store: default_postgres      # omit for filesystem (default); or a stores[] entry of kind sqlite|postgres
  root: ${QUACK_WORKSPACE_ROOT}/.quack/plugins
  seed:
    - github:fagerbergj/dotagents
    - github:DietrichGebert/ponytail@v4.9.0
```

`store` picks the registry backend (filesystem by default, or a `stores:` entry of kind `sqlite`/`postgres` - see [stores.md](stores.md#plugin-registry)); `root` is where clones live; `seed` lists entries inserted if absent at boot (the UI/REST own the list after that). A bare `plugins:` list is treated as `seed:`. Each entry is either `github:owner/repo[@ref][#path]` (cloned and kept up to date) or a local directory path (config-only, no clone). Full detail - entry syntax, naming, the bundled baseline, updates - is in [`agent-plugins.md`](../agent-plugins.md#the-registry).
