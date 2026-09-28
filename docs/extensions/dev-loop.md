# Developing an extension against quack

quack compiles its extensions in from tagged `quack-extensions` modules pinned in `go.mod`. While a change is in progress, build quack against your local checkout instead: no tag, no pin bump, no `go.mod` edit. Tag once, when the change is done.

Both targets below take `EXT`, the path to a `quack-extensions` checkout (any branch, dirty or not). Every module directory in it (`sdk/`, `github/`, `sleeper/`, ...) replaces its pinned version.

## Local binary

```bash
make dev-build EXT=../quack-extensions
go version -m quack | grep quack-extensions   # each module reports (devel)
./quack version                               # dev+ext.<sha of the checkout>
```

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

The checkout reaches the build as a BuildKit named context (`--build-context ext=...`), and `--build-arg QUACK_SRC=backend-src-ext` selects the Dockerfile stage that writes the `go.work`. A plain `docker build` never builds that stage: the default `QUACK_SRC=backend-src` compiles against the `go.mod` pins, and `.dockerignore` keeps any local `go.work` out of the context.

To exercise the image end to end, point the QA instance (see `AGENTS.md.local`) at the dev tag and recreate its container.

## Before merging the extension change

The `quack-compat` workflow in `quack-extensions` builds quack `main` against the PR's modules: `go build ./...`, `go vet ./...`, the tests of every quack package that imports an extension module, then `quack server validate config/quack.yaml`. Run the same check locally from the `quack-extensions` checkout:

```bash
tools/quack-compat.sh                  # clones quack main
tools/quack-compat.sh ../quack         # or uses an existing quack checkout
QUACK_REF=my-branch tools/quack-compat.sh
```

When the change needs a matching quack change, run the workflow manually with `quack_ref` set to that quack branch.

## Tag once

1. Merge the `quack-extensions` PR.
2. Tag each changed module once, e.g. `git tag sleeper/v0.7.0 && git push origin sleeper/v0.7.0`.
3. In quack, bump the pins in one PR: `go get github.com/fagerbergj/quack-extensions/sleeper@v0.7.0 && go mod tidy`.
