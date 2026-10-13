# extensions

The extension SDK for quack, plus quack's first-party extensions. Until 2026-10 these lived in `github.com/fagerbergj/quack-extensions` (archived); their history before the move is there. Full design: [issue #275](https://github.com/fagerbergj/quack/issues/275#issuecomment-5245275574). How to work on one, ship a plugin change, or add an extension: [docs/extensions/dev-loop.md](../docs/extensions/dev-loop.md).

## The invariant

**An extension reaches quack only through the SDK.** The SDK is self-contained (chi, slog, adk's `tool.Tool`); extensions import only the SDK, and quack imports the SDK for the seam types and each extension package once, as a blank import in `internal/serve/extensions_registry.go`, for registration. [`boundary_test.go`](boundary_test.go) enforces both directions. This is what lets an extension be written outside this repository against the SDK alone, compile into quack's binary, and still be enabled or left dormant per deployment by config alone.

A consequence: extensions shape a run only at dispatch time (`DispatchRequest`'s `Chat`/`Ask`/`Run`/`Delivery` groups). There are no agent-loop hooks - enforcement stays gate-owned inside quack.

A chat runs one run at a time. `Host.Dispatch` onto a chat whose run is still live dispatches nothing and returns an error wrapping `sdk.ErrRunInFlight`; treat it as busy, not as a host failure, and retry from that chat's `RunEnded` if the ask must not be lost.

A run whose node the user pauses ends with `sdk.RunPaused`, which is not final: keep whatever the run holds, because resuming the node delivers a second `RunEnded` with the real outcome.

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

## Out of process

Setting `exec: <binary>` in an `extensions.<name>` block runs that extension as a child process instead of calling its compiled-in factory; dropping the key puts it back in-process. The binary's `main` is one call to [`extproc.Main(name)`](sdk/extproc/serve.go) (see [usage/cmd/quack-ext-usage](usage/cmd/quack-ext-usage/main.go)), and the image ships `/quack-ext-usage`.

The wire is JSON-RPC 2.0, one JSON object per line on the child's stdin/stdout: `initialize` (config block, data dir, version, public URL, zone -> UI descriptor; the child runs the factory and any `Starter.Start` here) then one `http` call per request under `/<name>/`. quack keeps the session auth, strips `Authorization` and `Cookie`, and answers 502 while the child is down. Request and response bodies are capped at 16MiB: an oversized request gets 413, an oversized response 502. The child logs JSON to stderr, re-emitted in quack's log under `component=ext.<name>` (lines over 64KiB are truncated). quack restarts a crashed child with backoff (100ms doubling to 30s, reset once a child outlives 30s) and stops it on shutdown by closing its stdin, then SIGKILL after 5s. The child gets only PATH, HOME, TMPDIR, TZ, LANG, SSL_CERT_FILE, SSL_CERT_DIR and the HTTP(S)_PROXY/NO_PROXY variables from quack's environment.

Phase 1 limits: only authed routes are bridged, so no public webhooks. There are no tools, `RunEnded`, or `Host` callbacks such as `Dispatch` (which returns an error), no streaming responses, no request cancellation over the wire, and handlers see an empty `Host` and `RemoteAddr`. The child is not sandboxed the way plugin MCP servers are. Validation still runs the compiled-in factory, so an `exec` extension must also be compiled into quack.
