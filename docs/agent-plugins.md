# Agent Plugins in quack

quack loads plugins packaged per the [Agent Plugins](https://agent-plugins.org/)
specification, version 1.1.0. One plugin root can carry three things:

| Component | Location | Portable? |
| --- | --- | --- |
| Skills | `skills/` | Yes (spec §7.1) |
| MCP servers | `mcp.json` | Yes (spec §7.2) |
| quack extension declarations | `plugin.json` → `extensions["io.github.fagerbergj.quack"]` | No (spec §8) |

Roots are listed under the top-level `plugins:` key in quack.yaml. The old
`skills.plugins:` key still works and is read as a deprecated alias, but a
root contributes more than skills now, so the name no longer fits.

```yaml
plugins:
  - .agents/vendor/dotagents
  - .agents/vendor/ponytail
  - .agents/plugins/usage
```

## The namespace

quack's client-extension namespace is **`io.github.fagerbergj.quack`**.

The spec asks a client to base its namespace on a domain it controls (§8).
quack owns no registered domain; `github.com/fagerbergj` is the account that
owns both this repository and quack-extensions, so `io.github.fagerbergj` is
the reverse-domain root, with `.quack` scoping it to this client rather than
to its author personally.

Everything quack-specific lives under that key. `plugin.json` itself stays
strictly spec-shaped — the portable manifest schema is closed
(`additionalProperties: false`), so there is nowhere else it could legally go.

quack does **not** use a `io.github.fagerbergj.quack/` extension directory.
§8 permits manifest data alone, and nothing in v1 needs client-owned files.

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

Unknown fields inside the block are rejected. Validation and failure handling
inside a namespace belong to its owner (§8), and this block declares compiled
code, so quack is strict about it.

## Two seams

A plugin can extend quack in two ways, and which one applies is decided by
what the capability actually needs, not by preference.

### Tool-shaped capabilities → MCP servers (`mcp.json`)

Anything that is only "a tool the agent can call" is an out-of-process stdio
MCP server declared in the plugin's `mcp.json`, exactly as the spec defines
it. quack:

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

This half is portable on purpose. A quack plugin whose capability is a tool
is installable by any conformant Agent Plugins host, unchanged.

### Host-coupled surfaces → compiled-in Go modules

`RegisterRoutes`, `UI()`, `RunObserver`, `Starter`/`Stopper`, `Deliverer` and
`GitCredentialSource` mount HTTP handlers inside quack's own server, add nav
entries to its SPA, and observe run outcomes. None of that is expressible as
an MCP tool, and none of it is portable. Those stay `sdk.Extension`
implementations pinned in `go.mod` and blank-imported in
`internal/serve/extensions_registry.go`.

Go has no safe dynamic loading, and quack does not pretend otherwise. The
manifest does not load anything — it *declares*, and boot checks the
declaration against what the linker actually produced:

```
plugin "ghost" declares module "ghost"
(github.com/fagerbergj/quack-extensions/ghost), which is not linked into this
binary; add its blank import to internal/serve/extensions_registry.go
```

The manifest is documentation the compiler is checked against.

## Configuration

Plugin configuration stays in quack.yaml under `extensions:`, keyed by the
module's registration name. The Agent Plugins manifest defines no config
schema, and inventing one would only duplicate the Go struct each factory
already unmarshals into.

What the manifest does say is whether config is mandatory:

- `"config": "required"` — a module that appears under `extensions:` with an
  empty block fails the boot, naming the plugin. This is the fail-on-empty
  behaviour code-bearing extensions have always had, now declared instead of
  emerging from whichever factory happened to error first.
- `"config": "optional"` (or absent) — skill-only and MCP-only plugins keep
  warn-and-skip.

A module that is not mentioned under `extensions:` at all stays dormant in
both cases. Compiled-in but unconfigured has always meant "not running", and
that is unchanged.

## Failure philosophy

The spec's rule is that component failures are non-fatal (§11.3, §7.2.2), and
quack follows it everywhere the failure only costs a capability:

| Failure | Result |
| --- | --- |
| Root has no manifest, or an unreadable one | Warn, skip that root |
| Root has no `skills/` | Fine — §6.2 makes an absent location a non-error |
| `mcp.json` broken or version-mismatched | Warn, MCP disabled for that plugin only |
| One `mcp.json` server entry invalid | Warn, skip that entry |
| MCP server fails to start or list tools | Warn, its tools are absent |
| Foreign `extensions` namespace, any contents | Ignored, never validated (§8) |
| **Our namespace block malformed** | **Boot error** |
| **Declared module not linked** | **Boot error** |
| **`config: "required"` with an empty block** | **Boot error** |

The dividing line is whether the failure is silent. Losing a skill or a tool
is visible in the logs and degrades gracefully. Booting a server that promised
a module it does not have is a lie the operator only discovers when the
feature is missing at 3am, so those three cases stop the boot instead.

## Migration

Nothing breaks. `sdk.Extension` is unchanged, extension code still arrives
through `go.mod`, and an extension with no manifest works exactly as before —
the manifest adds a declaration, it does not become the load path.

`usage` is the first extension carrying one, at `.agents/plugins/usage/`.
It ships a manifest and nothing else: no skills, no MCP servers, just the
namespace block declaring the module and its config requirement. That
exercises every new path (namespace parsing, the linked-module check, the
config check, and the no-`skills/` fix) against a real extension without
touching the extension's code.

`github` is deliberately not converted. It is the largest and most coupled
extension — webhooks, GitHub App identity, its own persisted state — and
whatever the eventual shape, it should not be the thing proving the shape
works.

Manifests currently live in this repository under `.agents/plugins/` because
quack chooses which modules it links. When quack-extensions ships manifests
alongside its own modules, the vendored tree (`.agents/vendor/`, pinned in
`.agents/vendor/plugins.yaml`) takes over and these are removed. That means a
converted extension is pinned twice — a vendor ref for the manifest and skills,
a `go.mod` version for the code. The linked-module boot check is what keeps
the two pins from drifting apart silently.
