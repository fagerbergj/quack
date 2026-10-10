# Agent Plugins in quack

quack loads plugins packaged per the [Agent Plugins](https://agent-plugins.org/) specification, version 1.1.0. One plugin root can carry these things:

| Component | Location | Portable? |
| --- | --- | --- |
| Skills | `skills/` | Yes (spec §7.1) |
| MCP servers | `mcp.json` | Yes (spec §7.2) |
| quack extension declarations | `plugin.json` → `extensions["io.github.fagerbergj.quack"]` | No (spec §8) |
| Agent bundles | `agents/<bundle>/`, listed in the namespace block's `agents` | No - quack's own layout addition |
| Workflow shapes | `workflows/*.yaml`, listed in the namespace block's `workflows` | No - quack's own layout addition |

Skills are pulled from a dynamic **plugin registry** at run time (epic #1427): each plugin is a registry row, cloned to disk, fetched at boot and refreshable from the UI or REST with no rebuild. A fetched plugin's `mcp.json` loads exactly like a local root's - adding the row is the trust boundary, not a separate MCP approval step (see [Security](#security)). Compiled extension modules still come only from the binary, as described below.

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
- `seed` - applied at every boot (a Reload does not re-read quack.yaml). An absent name is inserted and marked `seeded`. A row that config owns - a local root, or a row seeding created - follows its seed entry: when the entry changes (a new ref, or a vendored local root moved to a `github:` entry), the row is replaced and the boot fetch moves the clone. A row added or re-POSTed over REST is operator-owned and never changed by seeding; a REST add of a seeded row's name takes it over. Any row whose entry equals its seed entry is marked seeded at boot, so a REST row that matches the seed is handed back to config then; to hold a row at the seed's current ref across later bumps, POST a different entry for it (e.g. `@<sha>`). The row's `seeded` field on `GET /api/v1/plugins` shows which rows config owns. A seed row deleted over REST comes back at the next boot while it stays in `seed`; a row added over REST is never removed. Setting `seed` **replaces** the stock defaults (dotagents, ponytail, usage) - it is not an extension of them; list the defaults explicitly if you still want them.

### Entry syntax

A seed entry, or an entry POSTed to `/api/v1/plugins`, is one of:

- **`github:owner/repo[@ref][#path]`** - a git-hosted plugin, cloned under `root`.
  - No `@ref`: **tracked** - follows the repo's default branch; `GET /api/v1/plugins/updates` and the Update button move it forward.
  - `@ref` (a tag, branch, or commit sha): **pinned** - only moves when the entry is re-POSTed at a different ref (REST). Editing a `plugins.seed` entry moves a seeded row at the next boot; a row last set over REST stays where the operator put it. A 40-hex sha is always reported as not behind.
  - `#path`: the plugin root is a subdirectory of the repo instead of its root. Relative, and rejected if it escapes the repo.
  - A trailing `.git` on the repo name is stripped.
  - Private repos: set `GITHUB_TOKEN` in the environment quack runs under; the token is passed to git via `GIT_CONFIG_*` as a header for `https://github.com/` URLs, never written to argv or persisted in the row. Registry git inherits only `PATH`, `TMPDIR`, the proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `ALL_PROXY`, either case) and the CA variables (`GIT_SSL_CAINFO`, `GIT_SSL_CAPATH`, `SSL_CERT_FILE`, `SSL_CERT_DIR`) from quack's environment, and runs with an empty per-call `HOME` and no system or global git config.
  - REST's `POST /api/v1/plugins` only accepts `github:` entries - a local root is config-only, added under `seed:`.
- **A local root** (anything not starting with `github:`) - a directory path, read in place with no clone and no fetch. This is the pre-registry `plugins:` list form; it is config-only, seeded at boot, and cannot be added, updated, or removed over REST (`DELETE /api/v1/plugins/{name}` answers `409`). A changed seed entry under its name replaces the row at boot. Resolved via a root `plugin.json` (Agent Plugins); else `.codex-plugin/plugin.json` (Codex, an explicit `skills` field); else skipped with a warning naming the root.

### Naming

Every registry row has a **name**, which prefixes its skills as `<name>:<skill>`:

- A `github:` entry's name is the repo component (`github:fagerbergj/dotagents` -> `dotagents`) - or, with a `#path`, the last path segment (`github:fagerbergj/quack-extensions#sleeper/plugin` -> `sleeper`; a trailing `plugin` names its parent, `#sleeper` -> `sleeper`), so one repo can host several plugins. Never `plugin.json`'s own `name` field.
- A local entry's name is the base name of its path (`/opt/checkouts/my-checkout` -> `my-checkout:<skill>`), also never the manifest's name.
- `update`, `updates` and `reload` are reserved outright: they collide with the fixed REST path segments `/api/v1/plugins/update`, `/api/v1/plugins/updates` and `/api/v1/plugins/reload`, and `POST /api/v1/plugins` refuses each name. `quack` is reserved from `POST /api/v1/plugins` the same way, but a `plugins.seed` entry may be named `quack` - that is how a fetched copy shadows the embedded baseline (see below). `DELETE /api/v1/plugins/quack` is refused either way, seed or REST: the embedded baseline itself is never removable.

A bare skill name (no `plugin:` prefix) - in an agent's `skills:` scope, or a boot-time lookup - resolves to the first plugin providing that name in seed order; rows added later over REST sort after the seed. An explicit `other:skill` outside an agent's scope is not-found, never silently substituted for an in-scope plugin's copy.

### The bundled baseline and shadowing

quack's shipped `skills/` tree, plus a tracked snapshot of dotagents' skills (`.agents/embedded/dotagents/skills`, pinned at the sha its `SOURCE.md` names), are `go:embed`ded into the binary and registered as the plugin `quack`, source `embedded` - no entry, no clone, always present, and the offline fallback if the registry has nothing else.

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

### Reload

Every add, update, update-all and delete ends in a **reload**, and `POST /api/v1/plugins/reload` runs one on its own. A reload re-runs boot's plugin pipeline with no restart and swaps the result in as a new roster *generation*:

1. List the registry, then resolve and admit every row exactly as boot does (see [Admission](#admission)).
2. Seed each plugin's agents and workflow shapes into a fresh copy of the config as it was before any plugin seeded into it, so a deployment's `agents.<name>:` override keeps winning and an edited shape replaces the old one.
3. Start MCP servers for the admitted plugins. A server whose plugin, revision (root and installed sha) and launch spec are unchanged keeps its running process; a new or changed one is spawned and its tools enumerated.
4. Build the agents, then swap in the skills, agents, planner table and workflow catalog together.

A run already in flight finishes on the generation it started on: its agents, tools and MCP servers stay as they were until that run ends, and a server the new generation no longer uses stops once the last such run finishes. New runs use the new generation. A `local` root's sha is always empty, so editing code under it does not restart its servers unless the `mcp.json` launch spec itself changes. A fetch checks the new sha out in place, so a still-running server of an old generation reads the new files from then on. `prompt.md` and `rubric.yaml` are still re-read from disk per round and per run.

The response is a report:

| Field | Meaning |
| --- | --- |
| `generation` | The generation now serving new runs; unchanged if nothing swapped |
| `agents` | `added`, `removed`, and `updated` (bundle or resolved config changed, with the new `bundle_hash`) |
| `workflows` | `added`, `updated`, `removed` shape names |
| `mcp_servers` | `started`, `reused`, `stopped`, each `plugin/server`; a changed server is both stopped and started |
| `failures` | `{plugin, member, stage, error}` for each piece that was dropped or that stopped the reload |

One broken piece costs only itself: a plugin whose agents or shapes fail to seed is dropped (stage `seed`), a `bundle:`-less override without `optional: true` that no plugin seeded is dropped (`config`, named with the plugin that last supplied it), an `mcp.json` entry that is invalid or a server that fails to start or list its tools is dropped (`mcp`, naming the server; no server name when the whole file was rejected), and a plugin agent that fails to build - bad `agent-card.json`, rubric, memory, model, or tools - is dropped (`agent`). A reload stops with **nothing swapped** and the previous generation still serving when the registry can't be read (`registry`), a `plugins.seed` row is refused (`admission`), a config-authored agent is left incomplete (`config`), a config-authored agent fails to build (`agent`; an `optional: true` one is dropped instead, but only when its tools are unknown), or the server is shutting down (`closed`). `POST /api/v1/plugins/reload` then answers `422` with the same report. The plugin endpoints carry the report as `reload`: add and update in their `200`/`201` body, or in the `422` body (with `error`) when that row was refused or the reload stopped - the row stays registered either way. `DELETE /api/v1/plugins/{name}` answers `200` with `{reload}`.

The wire row's `declares_mcp_servers` flags whether the plugin *currently* ships a server.

### Admission

A newly fetched or re-fetched plugin still runs the same checks as any other (linked module, `config: "required"`; see [Failure philosophy](#failure-philosophy)), but what a failure does depends on where the row came from:

- A **seed row** (`plugins.seed` / config) failing admission is a **boot error**, named, and stops the boot - the same "boot error" treatment every config-driven refusal has always had.
- A **REST-added row** failing admission is **dropped**, not fatal: `POST /api/v1/plugins` and `POST /api/v1/plugins/{name}/update` return `422` with the refusal, the row's `error` is persisted, and the rest of the roster (every other plugin) still loads - the plugin stays registered, visible with its error, just not contributing skills until fixed. A refusal hit during `POST /api/v1/plugins/update` (update-all) does not fail that call's `200`; it is reported per-row in the response body instead.

So a refused row never blocks any other row from admitting only when it was added or updated over REST; a refused seed row is fatal to boot, by design.

Seeding follows the same split at boot: a seed row whose agents or shapes fail to seed (a malformed `agent.yaml`, a shape naming an unknown agent) stops the boot, while a REST-added row's failure drops that plugin and logs it. A reload drops either kind and reports it (stage `seed`).

### Provenance

Every `agent.invoke` ledger entry for an ACP round records `plugins: [{name, sha}]` - every registered row plus the embedded `quack` baseline, each with the sha it was serving from (omitted for the embedded `quack` row and local roots, which have no sha), regardless of whether that round's agent actually used that plugin's skills. Native (non-ACP) rounds do not yet record this ([#1445](https://github.com/fagerbergj/quack/issues/1445)).

### Security

Adding a plugin - a `plugins.seed` entry, or `POST /api/v1/plugins` from the Plugins page - is the trust boundary. Once a plugin is registered, its `mcp.json` loads exactly like a local root's: quack does not ask for a second approval before spawning a server it declares. A registry plugin's clone is authored by pushing to its git remote, and quack never edits a clone's contents, only checks it out at a ref - so **a tracked (unpinned) `github:` entry can start running a different server the next time it's fetched, with no further click from the operator**. Pin any plugin that ships MCP servers (`github:owner/repo@ref`) so a push to its remote can't change what runs without an operator editing the row.

Every MCP server subprocess, fetched plugin or local root alike, gets read-only access to its own plugin root and a writable `PLUGIN_DATA` under the workspace root (`${workspace.root}/plugins/<name>`, outside the clone and outside `plugins.root`), through the same sandbox seam ACP subprocesses use (`workspace.WrapArgv`). Inside the pi ACP shim, skills are loaded by directory name on disk, so pi sees bare skill directory names (e.g. `format-markdown`), not plugin names and not the `plugin:skill` qualifier native `list_skills` shows.

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
| `agents` | no | Agent bundle names to seed - the only input to seeding. Each must be an actual bundle at `agents/<name>/agent-card.json`, or the plugin is refused naming the entry; a bundle present but absent from this list (including when the key is omitted entirely) is skipped and logged, not seeded. |
| `workflows` | no | Workflow shape names to seed, same contract as `agents` against `workflows/<name>.yaml` - whose internal `name:` must equal `<name>`, or the plugin is refused naming the file. |

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

`RegisterRoutes`, `UI()`, `RunObserver`, `Starter`/`Stopper`, `Deliverer`, `GitCredentialSource` and `ArtifactSchemas` mount HTTP handlers inside quack's own server, add nav entries to its SPA, observe run outcomes, and declare the JSON Schema an artifact kind must satisfy. None of that is expressible as an MCP tool, and none of it is portable. Those are `sdk.Extension` implementations pinned in `go.mod` and blank-imported in `internal/serve/extensions_registry.go`.

An extension implementing `ArtifactSchemas() map[string]json.RawMessage` names each artifact kind it reads and the draft 2020-12 JSON Schema that kind's content must satisfy. quack collects these at boot into one registry; two extensions declaring the same kind, or a schema that fails to compile, fails boot naming both. From then on, `write_artifact`/`write_<kind>`/`edit_artifact` (both the native tool surface and the ACP loopback MCP surface) refuse a write to a schema'd kind whose resulting content does not validate — nothing is stored, and the tool's error lists the violations. A gated node whose declared artifact kind has a schema also gets a deterministic `artifact_valid` criterion (see `docs/configuration/trust-gate.md`).

The `sdk.Host` quack passes each extension's factory carries `Location`, the user's configured zone from `timezone` (see `docs/configuration/index.md`). It is nil when `timezone` is unset, so an extension picks its own fallback instead of mistaking the server's zone for the user's.

Every extension tool call carries an `sdk.CallInfo`, read with `sdk.CallInfoFrom(ctx)`: the chat id (`ext:<extension>:<ChatRef.LocalID>` for a chat the extension dispatched), the chat's user, the run's allowed delivery kinds, and `ReadOnly`, set for a plan-only run that must not write or post. `AllowedDeliveryKinds` is the grant the trust gate applies, including the one a REST turn or nudge re-applies to an extension chat: nil means no grant governs the run and a non-nil empty list means nothing may be delivered. Inside a DAG node quack reads these from the node's own advisor-thread registration, so they survive the per-node A2A hop; if that registration is missing the call fails closed, with an empty grant and `ReadOnly` set. The gate enforces only deliveries staged through it, so for those `CallInfo` is advisory. A tool that acts directly, such as one that posts a comment itself, must enforce `CallInfo` on its own, because nothing downstream will.

Go has no safe dynamic loading, and quack does not pretend otherwise. The manifest does not load anything — it *declares*, and boot checks the declaration against what the linker actually produced:

```text
plugin "ghost" declares module "ghost"
(github.com/fagerbergj/quack-extensions/ghost), which is not linked into this
binary; add its blank import to internal/serve/extensions_registry.go
```

The manifest is documentation the compiler is checked against.

## Agent bundles and workflow shapes

A plugin can also ship its own agent roster and DAG shapes, seeded into `config.Config` at boot and on every [reload](#reload) - `agents:` and `workflows:` stay the deployment's own mechanisms; a plugin only adds entries to them before `buildAgents`/`workflowcatalog.FromConfig` run.

### Layout

```text
.agents/plugins/<name>/
  agents/<bundle>/
    agent-card.json   # required - the same A2A card any bundle carries
    prompt.md          # required
    rubric.yaml         # optional
    memory.md           # optional
    agent.yaml           # plugin-only: tools, skills, judge_rounds, context_window, model_role
  workflows/
    <shape-name>.yaml   # one file per shape, the exact `workflows:` entry schema
```

`internal/plugin.Resolve` sets `AgentsDir`/`WorkflowsDir` on a resolved `Plugin` when those directories exist, the same way `skills/` is found, but the namespace block's `agents`/`workflows` lists are what actually seed: listed and present seeds (unless a config-authored `agents:`/`workflows:` entry of the same name already exists, which still wins), listed and missing refuses the plugin, present but unlisted only warns and does not seed. A plugin that omits a list (or declares it empty) seeds nothing from that directory - the manifest is the only input, not a fallback to what's on disk. Two plugins listing the same agent or workflow name is also a refusal, naming both by their registry row name - manifest lists never silently merge. A subdirectory of `agents/` is a bundle only if it has an `agent-card.json`; every `*.yaml` file under `workflows/` is one shape whose internal `name:` must equal its filename.

These list/missing/collision checks run at plugin admission (`internal/serve`'s `admitPlugins`/`ResolveConfiguredPlugins`), not inside `Resolve` itself: a `plugins.seed` row's refusal still fails boot, but a REST-added row's refusal only drops that one plugin, the same #1430 rule every other plugin refusal already follows.

### `agent.yaml`

The plugin-bundle-only sibling of `agent-card.json`/`prompt.md` (a shipped bundle under `agents/` never carries this file):

```yaml
tools: [sleeper_roster, sleeper_matchup, current_date]
skills: [sleeper:start-sit]
judge_rounds: 1
context_window: 65536
model_role: researcher   # researcher | coder | judge
```

`model_role` substitutes for a "default model" concept config doesn't have (`models:` is keyed by resolved model id, not role): it maps to whichever `QUACK_RESEARCHER_MODEL`/`QUACK_CODER_MODEL`/`QUACK_JUDGE_MODEL` env var the deployment already set for its shipped agents of that role (`internal/config`'s `modelRoleEnv`). Provider defaults to the provider named `default` - the convention every shipped agent already uses; a deployment on a different provider name overrides it explicitly.

**The deployment must have the matching `QUACK_*_MODEL` env var set** for `model_role` to resolve to anything - there is no other default. An unset var resolves to an empty model, and boot or `server validate` fails naming the agent, exactly like a config-authored agent left with no `model:`. Set an explicit `model:` override (below) on a deployment with no matching `QUACK_*_MODEL` role.

### Precedence

A deployment's `agents.<name>:` entry - already in `config.Agents` before a plugin's bundles are seeded - overrides the plugin's `agent.yaml` defaults field by field (`provider`, `model`, `context_window`, `tools`, `skills`, `judge_rounds`, `memory`, `judge`, `acp`, `inputs`); an unset field keeps the plugin's own value. `bundle` and `optional: true` are never overridable - every plugin agent is implicitly optional, so one that fails to build for any reason (unresolved tools, a broken bundle, a bad model) is dropped from the roster with a warning, at boot and on a reload alike.

`tools:` **replaces** the plugin's list wholesale, never merges with it - the documented way to drop a tool whose backend the deployment doesn't run. A plugin agent whose tool fails to build because its backend isn't configured (e.g. `web_search` with no SearXNG url or Exa key) is dropped like any other build failure, so override `agents.<name>.tools` to the subset the deployment can actually build rather than lose the agent.

An override may leave `bundle:` (and `model:`, `provider:`) unset entirely, trusting the plugin to supply them - `config.LoadDeferringAgentCompleteness` defers requiring them until after plugin seeding runs, so the override alone is never rejected before the plugin gets a chance to complete it. A `bundle:`-less entry is only legal as a plugin override, so if no plugin seeds the name (the plugin is unfetched or refused, or its module is disabled), boot and every reload drop it after seeding and the rest of the roster still boots. Without `optional: true` that is unexpected: it logs an error (`no plugin seeded agent "x" ...; override dropped`), each reload reports it as a `config` failure, and `quack server validate` lists it as a warning. With `optional: true` the absence is expected and costs one warning log line - the GitHub extension's `code-implementer`/`code-reviewer`/`code-explorer` in `config/quack.yaml` are the shipped example, since a deployment with `extensions.github` off is expected to have no code agents.

A plugin workflow shape is appended to the raw `workflows:` list and validated by the exact same `validateWorkflows` path a config-authored shape gets (agent existence, bound-node artifact kinds, DAG acyclicity), one plugin's shapes at a time, so a shape naming a missing agent fails that plugin's seed naming the plugin, not just the shape (see [Admission](#admission) for whether that stops boot). A shape whose agent was later dropped from the roster - it failed to build - is left out of the workflow catalog rather than served dangling.

### Gating

A plugin whose `plugin.json` namespace block names a linked module (see [Host-coupled surfaces](#host-coupled-surfaces--compiled-in-go-modules) above) seeds its agents and shapes only when that module is configured under `extensions:` and not explicitly `enabled: false` - the same check `buildOneSDKExtension` makes before mounting the module itself. A plugin naming no module seeds unconditionally. Gating is absence, not a drop: a disabled extension's agents and shapes never enter `config.Agents`/`config.Workflows` in the first place, so there is nothing for the roster or planner table to warn about. Boot logs one info line per plugin that actually seeded something, naming the agents and shapes.

`quack server validate` resolves plugins the same way and lists each plugin's seeded agents and shapes in its output.

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

The `usage` and `github` manifests live in this repository under `.agents/plugins/`. Sleeper's plugin ships in quack-extensions beside its module, at `sleeper/plugin`, and is seeded as `github:fagerbergj/quack-extensions@sleeper/vX.Y.Z#sleeper/plugin`; a deployment whose `plugins.seed` lists that entry has its old local `sleeper` row moved to it at boot, and a later ref bump moves the seeded row the same way (see `seed` above). A deployment with its own `plugins.seed` must edit its sleeper entry itself; until then its local row points at a directory the image no longer ships, the plugin is skipped, and its sleeper overrides are dropped. A converted extension is pinned twice - the registry row's ref for the manifest, agents and skills, a `go.mod` version for the code - and one `sleeper/vX.Y.Z` tag releases both. `TestSleeperPluginSeedMatchesGoModPin` fails when `config/quack.yaml` and `go.mod` name different tags, and the linked-module boot check refuses a plugin whose module is missing.

Upgrading to a release with the `seeded` flag: every existing row starts unseeded, and boot marks the ones whose entry equals their seed entry. A `github:` row whose entry differs from its seed entry (moved over REST, or a seed edited while the old rule ignored edits) stays operator-owned and logs `plugin row was set over REST; not following plugins.seed` at every boot. Compare `GET /api/v1/plugins` entries with `plugins.seed` once after upgrading, and delete each mismatched row you want config to own; the next boot re-seeds it.
