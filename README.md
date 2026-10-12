# quack-extensions

The extension SDK for [quack](https://github.com/fagerbergj/quack), plus quack's first-party extensions. Full design: [issue #275](https://github.com/fagerbergj/quack/issues/275#issuecomment-5245275574) in the quack repo.

## The invariant

**Nothing in this repo may import `github.com/fagerbergj/quack`.** The SDK is self-contained (chi, slog, adk's `tool.Tool`); extensions import only the SDK. quack imports this repo - the SDK for the seam types, extension packages for registration - never the reverse. This is what lets an extension live outside quack's repo, compile into quack's binary, and still be enabled or left dormant per deployment by config alone.

A consequence: extensions shape a run only at dispatch time (`DispatchRequest`'s `Chat`/`Ask`/`Run`/`Delivery` groups). There are no agent-loop hooks - enforcement stays gate-owned inside quack.

## SDK versioning

`sdk`'s exported identifiers are the contract between quack and every extension; the doc comments in [`sdk/sdk.go`](sdk/sdk.go) describe the current surface and the `sdk/vX.Y.Z` tags carry its history. Optional capabilities are interfaces quack detects by type assertion, and `Host` func fields may be nil on an older host, so callers nil-check them.

## Module layout

Multi-module monorepo, one Go module per directory, each independently tagged (the otel-contrib shape):

```text
sdk/         github.com/fagerbergj/quack-extensions/sdk         - the Extension API
github/      github.com/fagerbergj/quack-extensions/github      - GitHub App integration and its code-agent plugin
noop/        github.com/fagerbergj/quack-extensions/noop        - proves the loop end to end (quack's e2e test fixture)
remarkable/  github.com/fagerbergj/quack-extensions/remarkable  - rmfakecloud document browser -> document-ingest
sleeper/     github.com/fagerbergj/quack-extensions/sleeper     - Sleeper fantasy-football tools, page and plugin
usage/       github.com/fagerbergj/quack-extensions/usage       - in-app Prometheus usage dashboard (inbound-only) and its manifest
tools/       CI-only gates (sloplint, coverdiff) and the quack-compat check
```

Future extensions land as sibling modules the same way.

## Tagging

Each module is tagged independently: `sdk/vX.Y.Z`, `noop/vX.Y.Z`, and so on - the module prefix distinguishes which module a tag versions, the way `go.opentelemetry.io/contrib` tags its many modules out of one repo. A change to only one module gets one tag; a coordinated SDK bump across every extension is one PR but still lands as separate tags per module.

**All modules stay on 0.x** (no stability promises) until quack itself reaches 1.0. Treat every SDK minor bump as potentially breaking and pin exact versions in consumers - don't rely on `^0.x` caret-style ranges.

No `go.work` is committed: each module resolves its siblings as normal tagged dependencies, the same way an external consumer would, so nothing about local development leaks into how quack (or anyone else) builds against this repo.

## Checking a change against quack

The `quack-compat` workflow builds quack `main` against a PR's module directories through a throwaway `go.work` (no tag, no pin bump). It runs `go build ./...` and `go vet ./...` over quack, the tests of every quack package that depends on a module from this repo, `quack server validate` on quack's shipped `config/quack.yaml` (plugin-declared modules are linked), and `quack server validate` on `tools/quack-compat.config.yaml`, which enables every extension with placeholder values so each Factory must accept its config, and seeds every `<module>/plugin/` from this checkout as a local plugin so each one must seed all the agents and workflows its `plugin.json` lists. The script also checks that every tool a plugin agent names without its module's prefix is in quack's builtin tool map. Every module directory except `sdk/` needs a block in that fixture, and every `plugin/` needs a `plugins.seed` entry. The workflow runs on PRs and `main` pushes that touch module code. Do not make it a required check: with its path filter it never reports on other PRs, which would then wait forever.

Run it locally with `tools/quack-compat.sh [quack-checkout]` (no argument clones quack at `$QUACK_REF`, default `main`). When a change needs a matching quack branch, the PR run fails against `main`; rerun it against that branch:

```bash
gh workflow run quack-compat.yaml --ref <this-repo-branch> -f quack_ref=<quack-branch>
```

`--ref` must be a branch of this repository; a fork's branch cannot be dispatched, so run the script locally for those. To run quack itself against a checkout, use quack's `make dev-build` / `make dev-image EXT=<this checkout>` ([docs](https://github.com/fagerbergj/quack/blob/main/docs/extensions/dev-loop.md)). Tag each changed module once, after merge; when `sdk` changed too, tag it first, bump the dependents' `sdk` require to that tag, then tag the dependents.

## How quack consumes this

quack's own `go.mod` pins the blessed extensions and a registry file blank-imports them (each extension package registers itself via `sdk.Register` from `init()`). Compiled but unconfigured is dormant; configured but not compiled is a loud startup error. Deployments enable and configure extensions through `extensions:` blocks in `quack.yaml`, which quack hands each extension's `Factory` as opaque config bytes. A module can also ship a declarative plugin (agents, skills, workflows) under `<module>/plugin/`, which quack fetches through its plugin registry as `github:fagerbergj/quack-extensions@<module>/vX.Y.Z#<module>/plugin`; see [sleeper/README.md](sleeper/README.md#plugin).

See the design doc's "Model" section for the full picture, including the default batteries-included image and the escape hatch for a minimal/custom build.
