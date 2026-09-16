# Agent Plugins in quack

quack loads plugins packaged per the [Agent Plugins](https://agent-plugins.org/) specification, version 1.1.0. One plugin root can carry three things:

| Component | Location | Portable? |
| --- | --- | --- |
| Skills | `skills/` | Yes (spec §7.1) |
| MCP servers | `mcp.json` | Yes (spec §7.2) |
| quack extension declarations | `plugin.json` → `extensions["io.github.fagerbergj.quack"]` | No (spec §8) |

Skills are pulled from a dynamic **plugin registry** at run time (epic #1427): each plugin is a registry row, cloned to disk, fetched at boot and refreshable from the UI or REST with no rebuild. A fetched plugin contributes its `skills/` tree only. Its `mcp.json` is ignored with a warning ([#1434](https://github.com/fagerbergj/quack/issues/1434)); MCP servers and compiled extension modules come from local roots and the binary, as described below.

## The registry

The top-level `plugins:` key in quack.yaml configures the registry:

```yaml
plugins:
  store: default_postgres      # omit for filesystem (default); or a stores[] entry of kind sqlite|postgres
  root: ${QUACK_WORKSPACE_ROOT}/.quack/plugins
  seed:
    - github:fagerbergj/dotagents
    - github:DietrichGebert/ponytail@v4.9.0
    - .agents/plugins/usage
```

- `store` - which registry backend holds the rows. Omitted or `""` uses the built-in filesystem backend (`<root>/<name>/entry.json`). Any other value must name a `stores:` entry of kind `sqlite` or `postgres`; that store's URL must not be empty (except under `quack sandbox`'s `LoadForSandbox`, which skips this and every other live-inference-plumbing check). A database-backed store holds rows in a `plugin_rows` table - the clones themselves still live on disk under `root`, exactly as the filesystem backend lays them out.
- `root` - where clones live. Defaults to `<workspace.root>/.quack/plugins` (a dot-dir so it never collides with repo checkouts on the same volume).
- `seed` - entries inserted into the registry if their name is absent at boot. After that, the UI and REST own the list; a config restart does not re-add a row someone removed, and does not remove a row someone added.

`plugins:` as a bare YAML list (the pre-registry local-root form) is treated as `seed:` with the filesystem backend:

```yaml
plugins:
  - .agents/vendor/dotagents
  - .agents/vendor/ponytail
  - .agents/plugins/usage
```

The old `skills.plugins:` key still works and is read as a deprecated alias for `plugins.seed` (a warning is logged). `plugins.seed` wins only when it is actually set; a `plugins:` block that sets only `store`/`root` with no `seed:` key still takes its seed list from `skills.plugins` if that is set.

### Entry syntax

A seed entry, or an entry POSTed to `/api/v1/plugins`, is one of:

- **`github:owner/repo[@ref][#path]`** - a git-hosted plugin, cloned under `root`.
  - No `@ref`: **tracked** - follows the repo's default branch; `GET /api/v1/plugins/updates` and the Update button move it forward.
  - `@ref` (a tag, branch, or commit sha): **pinned** - only moves when the entry is re-POSTed at a different ref (REST). Editing a `plugins.seed` entry does not move a row that already exists under that name: seeding inserts only absent names, so delete the row first (REST) and restart. A 40-hex sha is always reported as not behind.
  - `#path`: the plugin root is a subdirectory of the repo instead of its root. Relative, and rejected if it escapes the repo.
  - A trailing `.git` on the repo name is stripped.
  - Private repos: set `GITHUB_TOKEN` in the environment quack runs under; the token is passed to git via `GIT_CONFIG_*`, never written to argv or persisted in the row.
  - REST's `POST /api/v1/plugins` only accepts `github:` entries - a local root is config-only, added under `seed:`.
- **A local root** (anything not starting with `github:`) - a directory path, read in place with no clone and no fetch. This is the pre-registry `plugins:` list form; it is config-only, seeded at boot, and cannot be added, updated, or removed over REST. Resolved via a root `plugin.json` (Agent Plugins); else `.codex-plugin/plugin.json` (Codex, an explicit `skills` field); else skipped with a warning naming the root.

### Naming

Every registry row has a **name**, which prefixes its skills as `<name>:<skill>`:

- A `github:` entry's name is the repo component (`github:fagerbergj/dotagents` -> `dotagents`), never `plugin.json`'s own `name` field.
- A local entry's name is the base name of its path (`/opt/checkouts/my-checkout` -> `my-checkout:<skill>`), also never the manifest's name.
- `update` and `updates` are reserved outright: they collide with the fixed REST path segments `/api/v1/plugins/update` and `/api/v1/plugins/updates`, and `POST /api/v1/plugins` refuses either name. `quack` is reserved from `POST /api/v1/plugins` the same way, but a `plugins.seed` entry may be named `quack` - that is how a fetched copy shadows the embedded baseline (see below). `DELETE /api/v1/plugins/quack` is refused either way, seed or REST: the embedded baseline itself is never removable.

A bare skill name (no `plugin:` prefix) - in an agent's `skills:` scope, or a boot-time lookup - resolves to the first plugin providing that name in seed order; rows added later over REST sort after the seed. An explicit `other:skill` outside an agent's scope is not-found, never silently substituted for an in-scope plugin's copy.

### The bundled baseline and shadowing

quack's shipped `skills/` tree, plus the vendored dotagents copy, are `go:embed`ded into the binary and registered as the plugin `quack`, source `embedded` - no entry, no clone, always present, and the offline fallback if the registry has nothing else.

- A registry row also named `quack` (e.g. `github:fagerbergj/quack`) **shadows the embedded copy by qualified name**: `quack:plan-work` resolves to the fetched clone's copy, not the embedded one.
- Independently, an on-disk `dotagents` plugin under **any** registry name suppresses the embedded dotagents copy **by bare name**: with an on-disk `format-markdown` registered, a bare `format-markdown` lookup resolves to the on-disk copy, and the embedded `quack:format-markdown` is not served at all.

### Registry stores

Three backends implement the same `{List, Put, Delete}` registry interface:

| Backend | Selected by | Rows live in |
| --- | --- | --- |
| Filesystem (default) | `plugins.store` omitted | `<root>/<name>/entry.json`, one file per row |
| sqlite | `plugins.store: <name>`, that store's `kind: sqlite` | a `plugin_rows` table in the sqlite file |
| postgres | `plugins.store: <name>`, that store's `kind: postgres` | a `plugin_rows` table in that database |

All three keep clones on disk under `plugins.root` regardless of which one holds the rows - only the row bookkeeping (entry, resolved owner/repo/ref/path, installed sha, fetched-at, last error) moves.

### The update flow

`GET /api/v1/plugins/updates` compares each `github:`-sourced row's installed sha against its tracked ref (default-branch HEAD) or pinned ref, and reports which rows are behind; the UI calls this on page load. **Nothing changes without a click while the server is running**: `POST /api/v1/plugins/{name}/update` re-fetches one row regardless of whether it was reported behind; `POST /api/v1/plugins/update` fetches only the rows the check reported behind. A restart is the exception - boot re-fetches every `github:` row, so a tracked (unpinned) plugin moves to its default branch's current HEAD at boot even with no click.

A fetch failure - unreachable remote, moved/deleted ref, etc. - is stored on the row's `error` field and surfaced in the UI. It never fails boot or a run: the last good clone keeps serving until a later fetch succeeds. An admission failure (a declared module not linked, or similar) is handled per [Admission](#admission) below.

A fetch or update rebuilds the skill roster with no server restart: native agents' skill toolsets re-list on their next round, and an ACP-agent spawn rebuilds `skill_paths` from the registry on every round.

### Admission

A newly fetched or re-fetched plugin still runs the same checks as any other (linked module, `config: "required"`; see [Failure philosophy](#failure-philosophy)), but what a failure does depends on where the row came from:

- A **seed row** (`plugins.seed` / config) failing admission is a **boot error**, named, and stops the boot - the same "boot error" treatment every config-driven refusal has always had.
- A **REST-added row** failing admission is **dropped**, not fatal: `POST /api/v1/plugins` and `POST /api/v1/plugins/{name}/update` return `422` with the refusal, the row's `error` is persisted, and the rest of the roster (every other plugin) still loads - the plugin stays registered, visible with its error, just not contributing skills until fixed. A refusal hit during `POST /api/v1/plugins/update` (update-all) does not fail that call's `200`; it is reported per-row in the response body instead.

So a refused row never blocks any other row from admitting only when it was added or updated over REST; a refused seed row is fatal to boot, by design.

### Provenance

Every `agent.invoke` ledger entry for an ACP round records `plugins: [{name, sha}]` - every registered row plus the embedded `quack` baseline, each with the sha it was serving from (omitted for the embedded `quack` row and local roots, which have no sha), regardless of whether that round's agent actually used that plugin's skills. Native (non-ACP) rounds do not yet record this ([#1445](https://github.com/fagerbergj/quack/issues/1445)).

### Replay

A replay reads the `plugins` provenance recorded on the run's `agent.invoke` entries and pins each plugin's skill text to its recorded sha, served via `git show <sha>:<path>` against the clone's own history - not whatever the live clone currently holds, and never whether that plugin was admitted live. A plugin recorded with no sha (embedded/local) is served from the live source, scoped to just that plugin's skills, since it carries no sha to pin.

- A **missing clone or unknown sha** refuses at load, naming the plugin and the sha it cannot find.
- A plugin whose tree at the recorded sha carries no `plugin.json` or `.codex-plugin/plugin.json` is skipped, not refused, mirroring live [Admission](#admission) - decided from the sha's own tree, not from whether the plugin admitted live.
- Booting into a replay skips seeding, each row's `git fetch`, and persisting an admission refusal onto a row - so a replay makes no network calls and adds or rewrites no registry row, even on a shared sqlite/postgres registry. `git show`/`git ls-tree` against clones already on disk still run locally to serve each recorded sha.
- While a replay bundle has the roster pinned, every mutating plugin route (`POST /api/v1/plugins`, delete, update, update-all) returns **409**, naming the bundle - a replay in progress cannot have its pinned plugins moved out from under it.
- **ACP fork mode** is different: once a round diverges, the shim spawns a real subprocess against the *live* clone, not the recorded sha. Construction refuses up front if any recorded plugin's installed sha differs from what the registry currently has, or if the plugin is no longer registered at all - a live fork spawn cannot silently serve different skill text than the recording did. Pure (non-fork) replay never spawns a subprocess, so this check doesn't apply to it. To fork-replay against the plugin state as recorded, re-pin the plugin to that sha before starting the replay: `POST /api/v1/plugins` with `github:owner/repo@<sha>`, which checks the clone out at that sha and records it as the installed sha. Through config alone: `DELETE /api/v1/plugins/{name}`, then set the seed entry to `@<sha>` and restart (an existing row is never re-pinned by a seed edit).

### Security

A registry plugin's clone is authored by pushing to its git remote; quack never edits a clone's contents, it only checks it out at a ref. Sandboxed children (ACP subprocesses, plugin MCP servers from local roots) get read-only access to `plugins.root`, same as any other plugin root; a fetched plugin's `mcp.json` is ignored, so no MCP server is ever spawned against a registry clone. Inside the pi ACP shim, skills are loaded by directory name on disk, so pi sees bare skill directory names (e.g. `format-markdown`), not plugin names and not the `plugin:skill` qualifier native `list_skills` shows.

## The namespace

quack's client-extension namespace is **`io.github.fagerbergj.quack`**.

The spec asks a client to base its namespace on a domain it controls (§8). quack owns no registered domain; `github.com/fagerbergj` is the account that owns both this repository and quack-extensions, so `io.github.fagerbergj` is the reverse-domain root, with `.quack` scoping it to this client rather than to its author personally.

Everything quack-specific lives under that key. `plugin.json` itself stays strictly spec-shaped — the portable manifest schema is closed (`additionalProperties: false`), so there is nowhere else it could legally go.

quack does **not** use a `io.github.fagerbergj.quack/` extension directory. §8 permits manifest data alone, and nothing in v1 needs client-owned files.

## The namespace block

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json",
  "name": "usage",
  "version": "0.1.0",
  "description": "In-app usage dashboard backed by Prometheus.",
  "license": "MIT",
  "repository": "https://github.com/fagerbergj/quack-extensions",
  "keywords": ["quack", "observability", "usage"],
  "extensions": {
    "io.github.fagerbergj.quack": {
      "schemaVersion": 1,
      "modules": [
        {
          "name": "usage",
          "path": "github.com/fagerbergj/quack-extensions/usage"
        }
      ],
      "config": "required"
    }
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `schemaVersion` | yes | Must be `1`. Any other value is a boot error. |
| `modules` | no | Host-coupled Go modules this plugin declares. `name` is the `sdk.Register` name; `path` is the Go import path, carried so a failure can name the import to add. |
| `config` | no | `"required"` or `"optional"` (default). See below. |

Unknown fields inside the block are rejected. Validation and failure handling inside a namespace belong to its owner (§8), and this block declares compiled code, so quack is strict about it.

## Two seams

A plugin can extend quack in two ways, and which one applies is decided by what the capability actually needs, not by preference.

### Tool-shaped capabilities → MCP servers (`mcp.json`)

Anything that is only "a tool the agent can call" is an out-of-process stdio MCP server declared in the plugin's `mcp.json`, exactly as the spec defines it. quack:

- reads only `mcp.json` at the plugin root, and only with the recognized
  `$schema` (§7.2.1);
- supports the `stdio` transport. `streamable-http` and `sse` entries are
  valid but skipped with a warning, which §7.2.2 rule 4 makes the conformant
  response;
- expands `${PLUGIN_ROOT}` and `${PLUGIN_DATA}` in `args`, `env` values, and
  `cwd`, once and non-recursively (§9.2);
- supplies `PLUGIN_ROOT` and `PLUGIN_DATA` itself and rejects any entry whose
  `env` tries to set them (§9.1);
- creates `PLUGIN_DATA` at `<workspace.root>/plugins/<plugin name>` before
  launching, and never deletes it on its own;
- spawns the process through the same sandbox seam ACP workers run inside
  (`workspace.WrapArgv`) — the plugin root read-only, `PLUGIN_DATA` writable,
  a hermetic `PATH`, nothing else reachable.

### One deliberate divergence: `cwd`

§7.2.1 defaults a server's working directory to the plugin root and permits `${PLUGIN_ROOT}`-rooted values. quack instead puts `cwd` inside `PLUGIN_DATA` always, and refuses a `cwd` that names the plugin root.

A sandboxed child's own working directory is necessarily writable — under landlock the work grant *is* the cwd, and landlock unions per-path rules, so listing the root as read-only alongside it would not take the write away. A server that can write its own root can rewrite the `skills/` that get injected into agent prompts, and its own `mcp.json` across restarts. Read-only root wins over the cwd default.

The blast radius is small: the root stays readable, so `./bin/...` commands, `${PLUGIN_ROOT}` arguments, and `${PLUGIN_ROOT}` env values all work unchanged. Only the working directory differs.

This half is portable on purpose. A quack plugin whose capability is a tool is installable by any conformant Agent Plugins host, unchanged.

### Host-coupled surfaces → compiled-in Go modules

`RegisterRoutes`, `UI()`, `RunObserver`, `Starter`/`Stopper`, `Deliverer` and `GitCredentialSource` mount HTTP handlers inside quack's own server, add nav entries to its SPA, and observe run outcomes. None of that is expressible as an MCP tool, and none of it is portable. Those are `sdk.Extension` implementations pinned in `go.mod` and blank-imported in `internal/serve/extensions_registry.go`.

Go has no safe dynamic loading, and quack does not pretend otherwise. The manifest does not load anything — it *declares*, and boot checks the declaration against what the linker actually produced:

```text
plugin "ghost" declares module "ghost"
(github.com/fagerbergj/quack-extensions/ghost), which is not linked into this
binary; add its blank import to internal/serve/extensions_registry.go
```

The manifest is documentation the compiler is checked against.

## Configuration

Plugin configuration stays in quack.yaml under `extensions:`, keyed by the module's registration name. The Agent Plugins manifest defines no config schema, and inventing one would only duplicate the Go struct each factory already unmarshals into.

What the manifest does say is whether config is mandatory:

- `"config": "required"` — a module that appears under `extensions:` with an
  empty block fails the boot, naming the plugin. This is the fail-on-empty
  behaviour code-bearing extensions have always had, now declared instead of
  emerging from whichever factory happened to error first.
- `"config": "optional"` (or absent) — skill-only and MCP-only plugins keep
  warn-and-skip.

A module that is not mentioned under `extensions:` at all stays dormant in both cases. Compiled-in but unconfigured has always meant "not running", and that is unchanged.

## Failure philosophy

The spec's rule is that component failures are non-fatal (§11.3, §7.2.2), and quack follows it everywhere the failure only costs a capability:

| Failure | Result |
| --- | --- |
| Root has no manifest, or an unreadable one | Warn, skip that root |
| Root has no `skills/` | Fine — §6.2 makes an absent location a non-error |
| `mcp.json` broken or version-mismatched | Warn, MCP disabled for that plugin only |
| One `mcp.json` server entry invalid | Warn, skip that entry |
| MCP server fails to start or list tools | Warn, its tools are absent |
| MCP server hangs on connect or listing | Warn after 20s, its tools are absent |
| Foreign `extensions` namespace, any contents | Ignored, never validated (§8) |
| **Our namespace block malformed** | **Boot error for a `plugins.seed` row; `422` and dropped, rest of the roster still loads, for a REST-added row** |
| **Declared module not linked** | **Boot error for a `plugins.seed` row; `422` and dropped, rest of the roster still loads, for a REST-added row** |
| **`config: "required"` with an empty block** | **Boot error for a `plugins.seed` row; `422` and dropped, rest of the roster still loads, for a REST-added row** |

The dividing line is whether the failure is silent - and, for the plugin registry, whether it came from config or from the UI/REST. Losing a skill or a tool is visible in the logs and degrades gracefully. A `plugins.seed` row promising a module it does not have is a lie the operator only discovers when the feature is missing at 3am, so that stops the boot; the same refusal from a REST-added row is instead reported on the spot (`422`, error persisted on the row) and does not take the rest of the roster down with it.

## Migration

Nothing breaks. `sdk.Extension` is unchanged, extension code still arrives through `go.mod`, and an extension with no manifest works exactly as before — the manifest adds a declaration, it does not become the load path.

`usage` is the first extension carrying one, at `.agents/plugins/usage/`. It ships a manifest and nothing else: no skills, no MCP servers, just the namespace block declaring the module and its config requirement. That exercises every new path (namespace parsing, the linked-module check, the config check, and the no-`skills/` fix) against a real extension without touching the extension's code.

`github` is deliberately not converted. It is the largest and most coupled extension — webhooks, GitHub App identity, its own persisted state — and whatever the eventual shape, it should not be the thing proving the shape works.

Manifests currently live in this repository under `.agents/plugins/` because quack chooses which modules it links. When quack-extensions ships manifests alongside its own modules, the vendored tree (`.agents/vendor/`, pinned in `.agents/vendor/plugins.yaml`) takes over and these are removed. That means a converted extension is pinned twice — a vendor ref for the manifest and skills, a `go.mod` version for the code. The linked-module boot check is what keeps the two pins from drifting apart silently.
