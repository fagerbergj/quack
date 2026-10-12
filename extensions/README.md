# extensions

The extension SDK for quack, plus quack's first-party extensions. Until 2026-10 these lived in `github.com/fagerbergj/quack-extensions`; its history was imported here unsquashed, so `git log -- extensions/` reaches back past the move. Full design: [issue #275](https://github.com/fagerbergj/quack/issues/275#issuecomment-5245275574). How to work on one, ship a plugin change, or add an extension: [docs/extensions/dev-loop.md](../docs/extensions/dev-loop.md).

## The invariant

**An extension reaches quack only through the SDK.** The SDK is self-contained (chi, slog, adk's `tool.Tool`); extensions import only the SDK, and quack imports the SDK for the seam types and each extension package once, as a blank import in `internal/serve/extensions_registry.go`, for registration. [`boundary_test.go`](boundary_test.go) enforces both directions. This is what lets an extension be written outside this repository against the SDK alone, compile into quack's binary, and still be enabled or left dormant per deployment by config alone.

A consequence: extensions shape a run only at dispatch time (`DispatchRequest`'s `Chat`/`Ask`/`Run`/`Delivery` groups). There are no agent-loop hooks - enforcement stays gate-owned inside quack.

## SDK versioning

`sdk`'s exported identifiers are the contract between quack and every extension; the doc comments in [`sdk/sdk.go`](sdk/sdk.go) describe the current surface, and the `sdk/vX.Y.Z` tags from the old repository carry its history. Optional capabilities are interfaces quack detects by type assertion, and `Host` func fields may be nil on an older host, so callers nil-check them.

`sdk/` is its own module, `github.com/fagerbergj/quack/extensions/sdk`, so an outside extension can depend on it alone; quack's `go.mod` replaces it with `./extensions/sdk`, so an SDK change and its callers land in one commit. A release for outside authors is an `extensions/sdk/vX.Y.Z` tag. It stays on 0.x until quack reaches 1.0: treat every minor bump as potentially breaking and pin exact versions.

## Layout

```text
sdk/         github.com/fagerbergj/quack/extensions/sdk (own module) - the Extension API
github/      GitHub App integration and its code-agent plugin
noop/        proves the loop end to end (quack's e2e test fixture)
remarkable/  rmfakecloud document browser -> document-ingest, and that workflow's plugin
sleeper/     Sleeper fantasy-football tools, page and plugin
usage/       in-app Prometheus usage dashboard (inbound-only) and its manifest
```

Every directory but `sdk/` is a package of quack's own module, `github.com/fagerbergj/quack/extensions/<name>`, built and tested with the rest of quack.

## How quack consumes this

Each extension package registers itself via `sdk.Register` from `init()`, and `internal/serve/extensions_registry.go` blank-imports it. Compiled but unconfigured is dormant; configured but not compiled is a loud startup error. Deployments enable and configure extensions through `extensions:` blocks in `quack.yaml`, which quack hands each extension's `Factory` as opaque config bytes.

A module can also ship a declarative plugin (agents, skills, workflows) under `<name>/plugin/`. quack fetches it through its plugin registry as `github:fagerbergj/quack@plugin/<name>/vX.Y.Z#extensions/<name>/plugin`, so a plugin change ships with a `plugin/<name>/vX.Y.Z` tag and a seed bump rather than an image; see [sleeper/README.md](sleeper/README.md#plugin).
