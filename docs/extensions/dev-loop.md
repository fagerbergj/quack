# Developing an extension against quack

quack compiles its extensions in from tagged `quack-extensions` modules pinned in `go.mod`. While a change is in progress, build quack against your local checkout instead: no tag, no pin bump, no `go.mod` edit. Tag once, when the change is done.

Both targets below take `EXT`, the path to a `quack-extensions` checkout (any branch, dirty or not). Every module directory in it (`sdk/`, `github/`, `sleeper/`, ...) replaces its pinned version.

## Local binary

```bash
make dev-build EXT=../quack-extensions
go version -m quack | grep quack-extensions   # each module reports (devel)
./quack version                               # dev+ext.<sha of the checkout>
```

`dev-build` compiles Go only, so the binary serves the SPA placeholder; run `make frontend-build` once first if you need the web UI. `EXT` must contain at least one `*/go.mod` module directory, or the target stops before building.

`dev-build` writes a throwaway workspace at `.dev/go.work` and builds with `GOWORK` pointed at it. Plain `go` and `make` commands never read that file, so `make test` still tests the pinned versions. To run quack's own tests against the checkout, point `GOWORK` at it:

```bash
GOWORK=$PWD/.dev/go.work go test ./internal/serve/
```

## Docker image

```bash
make dev-image EXT=../quack-extensions             # tags quack:dev
make dev-image EXT=../quack-extensions DEV_IMAGE=quack:dev-sleeper
docker run --rm quack:dev version
```

The checkout reaches the build as a BuildKit named context (`--build-context ext=...`, `.git` excluded), and `--build-arg QUACK_SRC=backend-src-ext` selects the Dockerfile stage that writes the `go.work`. A plain `docker build` never builds that stage: the default `QUACK_SRC=backend-src` compiles against the `go.mod` pins, and `.dockerignore` keeps any local `go.work` out of the context.

To exercise the image end to end, point your QA or staging instance at the dev tag and recreate its container.

## Declarative plugins

`EXT` replaces Go modules only. A module's `plugin/` directory (agents, skills, workflows) reaches quack through the plugin registry, pinned in `config/quack.yaml`'s `plugins.seed`. To try a plugin change on a dev instance, seed its directory as a local root. A local row is named after its directory, so link it under the plugin's name first (`ln -s ~/quack-extensions/sleeper/plugin /opt/plugins/sleeper`, then seed `/opt/plugins/sleeper` in place of the `github:` entry). The seeded `github:` row and its clone are replaced by the local row at boot; switching the seed back replaces the local row and clones the plugin again. Or push a branch and add `github:fagerbergj/quack-extensions@<branch>#sleeper/plugin` from the Plugins page. That takes the row over from `plugins.seed`; add the seed entry back the same way when done, and config owns it again from the next boot.

## Before merging the extension change

The `quack-compat` workflow in `quack-extensions` builds quack against the PR's module directories through a throwaway `go.work`. It checks:

- `go build ./...` and `go vet ./...` over all of quack (vet type-checks every test file against the local modules).
- `go test` on every quack package that imports an extension module directly or transitively.
- `quack server validate config/quack.yaml`: quack's shipped config parses, and each plugin that declares a module finds it linked.
- `quack server validate` on `quack-extensions/tools/quack-compat.config.yaml`: a fixture in `quack-extensions` that enables every extension with placeholder values, so each extension's Factory must accept its documented config. `server validate` runs each enabled extension's Factory as boot does, but against a throwaway data directory because some Factories open their stores eagerly; it never calls `Start`.
- The same fixture seeds each module's `plugin/` directory from the checkout, and the script fails unless every agent and workflow its `plugin.json` lists is seeded.

It runs on pull requests and pushes to `main` that touch module code. Run the same check locally from the `quack-extensions` checkout:

```bash
tools/quack-compat.sh                  # clones quack main
tools/quack-compat.sh ../quack         # or uses an existing quack checkout
QUACK_REF=my-branch tools/quack-compat.sh
```

When the change needs a matching quack change, the PR run against quack `main` fails. Rerun the workflow against the quack branch that adapts to it:

```bash
gh workflow run quack-compat.yaml -R fagerbergj/quack-extensions --ref <extensions-branch> -f quack_ref=<quack-branch>
```

`--ref` must be a branch in `fagerbergj/quack-extensions` itself; a fork's branch cannot be dispatched, so run `quack-extensions/tools/quack-compat.sh` locally for those.

## Tag once

1. Merge the `quack-extensions` PR.
2. Tag each changed module once, e.g. `git tag sleeper/v0.7.0 && git push origin sleeper/v0.7.0`.
3. In quack, bump the pins in one PR: `go get github.com/fagerbergj/quack-extensions/sleeper@v0.7.0 && go mod tidy`. For a module that ships a `plugin/`, move its `plugins.seed` ref in `config/quack.yaml` to the same tag in that PR; a deployment's seeded row follows at its next restart.

When the change spans `sdk` and an extension that uses it, order matters: the `go.work` builds above use the local `sdk` whatever each extension's `go.mod` requires, so the tags must reproduce that. Tag `sdk` first, bump the dependent modules' `require github.com/fagerbergj/quack-extensions/sdk` to that tag in `quack-extensions`, merge, then tag the dependents and bump quack's pins.
