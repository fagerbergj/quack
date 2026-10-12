# Developing an extension

Extensions live in this repository under `extensions/`, one directory per extension, and build as packages of quack's own module: `extensions/sleeper` is `github.com/fagerbergj/quack/extensions/sleeper`. A change to an extension and the quack code it needs lands in one pull request, and `go build ./...` and `make test` cover both. There is nothing to tag or pin for Go code.

## Layout and the SDK boundary

```text
extensions/sdk/         the Extension API; its own module, github.com/fagerbergj/quack/extensions/sdk
extensions/<name>/      one extension package, registered with sdk.Register from init()
extensions/<name>/plugin/   optional declarative plugin: agents, skills, workflows (see below)
extensions/<name>/cmd/      optional QA tools, e.g. github's and remarkable's qa-mock
```

`extensions/sdk` keeps its own `go.mod` so an extension written outside this repository can depend on the SDK alone. quack's `go.mod` replaces it with `./extensions/sdk`, so an SDK change and its callers still land in one commit. If an outside author needs a release of it, tag `extensions/sdk/vX.Y.Z` (Go's tag form for a nested module); quack's own build never reads that tag.

An extension reaches core only through the SDK. `extensions/boundary_test.go` fails when a file under `extensions/` imports anything from quack outside `extensions/`, and when core code imports an extension package anywhere but the blank imports in `internal/serve/extensions_registry.go`. Core tests may import an extension to check core's policy against it.

Build and test as usual:

```bash
go build ./...
go test ./extensions/...           # the merged extension packages
go -C extensions/sdk test ./...    # the SDK module (make test runs both)
```

## Declarative plugins

A module's `plugin/` directory reaches a running quack through the plugin registry, not the binary: `plugins.seed` in `config/quack.yaml` pins each one as `github:fagerbergj/quack@plugin/<name>/vX.Y.Z#extensions/<name>/plugin`. The image carries no plugin files, so a prompt, rubric, skill or workflow change ships with a tag and a seed bump, without an image.

To try a plugin change on a dev instance, seed its directory as a local root. A local row is named after its directory, so link it under the plugin's name first (`ln -s ~/quack/extensions/sleeper/plugin /opt/plugins/sleeper`, then seed `/opt/plugins/sleeper` in place of the `github:` entry). The seeded `github:` row and its clone are replaced by the local row at boot; switching the seed back replaces the local row and fetches the plugin again. Or push a branch and add `github:fagerbergj/quack@<branch>#extensions/sleeper/plugin` from the Plugins page. That takes the row over from `plugins.seed`; add the seed entry back the same way when done, and config owns it again from the next boot.

## Shipping a plugin change

1. Edit the files under `extensions/<name>/plugin/`, bump `version` in its `plugin.json`, and merge.
2. Tag the merge commit `plugin/<name>/vX.Y.Z` and push the tag: `git tag plugin/github/v0.2.0 <sha> && git push origin plugin/github/v0.2.0`. `.github/workflows/cd.yaml` builds images only for `main` and `v*.*.*` tags, and a `plugin/` tag matches neither, so tagging builds nothing.
3. Move the seed pin to the new tag: `config/quack.yaml` for the shipped default, and each deployment's own `plugins.seed`.
4. Restart, or apply it live by re-sending the row: `quack api POST /api/v1/plugins -d '{"entry":"github:fagerbergj/quack@plugin/github/v0.2.0#extensions/github/plugin"}'` fetches the tag and reloads. A row set over REST belongs to the operator until a boot finds it equal to its seed entry again, so edit the seed too. At boot, a row seeding created follows its changed seed entry, including a move from the old `github:fagerbergj/quack-extensions@...` entries.

Sleeper's agents name the sleeper module's Go tools, which ship in the image. Tag `plugin/sleeper/*` only at a commit whose tools the running image already carries, or a plugin agent that names a missing tool is dropped. The github, usage and remarkable plugins name no module tools.

Two tests guard what a tag ships: `TestServerValidate_EveryExtensionAndPlugin` (`cmd/quack/extensions_validate_test.go`) runs `quack server validate` over `cmd/quack/testdata/every-extension.yaml`, which enables every linked extension with placeholder values so each Factory must accept its documented config, seeds every `extensions/*/plugin`, and fails unless each plugin seeds every agent and workflow its `plugin.json` lists. `TestExtensionPluginAgentsNameExistingBuiltins` (`internal/tools/plugin_tools_test.go`) fails when a plugin agent names a tool that is neither a quack builtin nor its own module's.

## Adding an extension

1. Create `extensions/<name>/` as a package that imports only `github.com/fagerbergj/quack/extensions/sdk` and calls `sdk.Register("<name>", factory)` from `init()`.
2. Blank-import it in `internal/serve/extensions_registry.go`. Compiled but unconfigured is dormant; a deployment enables it with an `extensions.<name>:` block.
3. Add an `extensions.<name>` block to `cmd/quack/testdata/every-extension.yaml`; the validate test fails for any linked extension without one.
4. For a plugin, add `extensions/<name>/plugin/plugin.json` with the `io.github.fagerbergj.quack` namespace block naming the module (see [Agent Plugins](../agent-plugins.md#the-namespace-block)), list it under the fixture's `plugins.seed`, cut its first `plugin/<name>/v0.1.0` tag after merge, and seed it in `config/quack.yaml`.
5. Document its config in `docs/extensions/<name>.md` and link it from [docs/README.md](../README.md).
