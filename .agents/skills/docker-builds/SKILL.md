---
name: docker-builds
description: |
  Standards and gotchas for the quack container build - Dockerfile, docker-compose.yml, and
  .dockerignore. Covers fast builds (layer order least→most volatile, BuildKit cache mounts, multi-stage
  parallelism), small/safe images (distroless base, non-root, static binary), the pin-by-maturity rule
  (mutable minor tags for this registry-less self-hosted stack, digests only for published images), and
  quack's conventions: the 3-stage SPA-embed flow (frontend dist embedded in the Go binary, then
  distroless:nonroot), CGO_ENABLED=0 + -trimpath -ldflags="-s -w", logs to stdout, build via make
  docker-up. Use when adding to, reviewing, or debugging the Dockerfile, docker-compose.yml, or
  .dockerignore - choosing a base image, ordering layers, adding a cache mount, or wiring a service.
  Do NOT use for Kubernetes/Helm manifests, CI workflow YAML, or non-container build tooling.
license: MIT
metadata:
  author: jason
  version: "1.0"
---

# Docker Builds - Quack

## Overview

How quack's container build is structured and why. One multi-stage `Dockerfile` produces a single static binary in a distroless runtime; `docker-compose.yml` wires it to Postgres, SearXNG, and crawl4ai for local/self-hosted use; `.dockerignore` trims the build context. This skill governs the *decisions* (base image, layer order, what to cache, how far to pin); concrete snippets live in `references/recipes.md`.

## When to Use

- Editing the `Dockerfile`, `docker-compose.yml`, or `.dockerignore`.
- Reviewing a diff that touches any of them (a new base image, a `RUN`, a service).
- Diagnosing a slow build, a bloated image, or a "file missing in the container" error.

## When NOT to Use

- Kubernetes / Helm / k8s `securityContext` - out of scope (no orchestrator today).
- `.github/workflows/` CI YAML - different concern.
- Non-container build tooling (`make build`, the Go/npm toolchains themselves).

## The two goals

**1. Fast builds - maximise cache hits, do redundant work once.**

- **Order layers least- → most-frequently-changed.** Dependency manifests + install (`package.json`/`go.mod` → `npm ci`/`go mod download`) come *before* `COPY . .`, so editing source doesn't reinstall dependencies. This is already correct in the Dockerfile - preserve it.
- **BuildKit cache mounts** (`--mount=type=cache`) persist a package manager's *download* cache across builds without committing it to a layer. Quack mounts `/root/.npm` (npm), `/go/pkg/mod` (go mod), and `/root/.cache/go-build` (go build). Add one when a `RUN` re-downloads/re-compiles the same artifacts every build.
- **Multi-stage parallelism** - independent stages (the `frontend` and `backend` stages) build concurrently under BuildKit. Keep stages independent where possible.

**2. Small, safe images - ship only the binary, run unprivileged.**

- **Multi-stage discards build tooling.** The final stage `COPY --from`s only the compiled artifacts; node, the Go toolchain, and all caches stay in earlier stages. This is the #1 size lever and is already in place.
- **Distroless + non-root.** The runtime is `gcr.io/distroless/static-debian12:nonroot` - no shell, no package manager, runs as UID 65532. Safe because the server writes nothing to disk at runtime (state → Postgres) and binds the unprivileged port 8080.
- **Static binary.** `CGO_ENABLED=0` + `-trimpath -ldflags="-s -w"` → a self-contained, symbol-stripped binary that needs no libc, which is what lets the base be `static-debian12`.

## Pin-by-maturity (the tag rule)

Match pinning to the image's release maturity, not to a blanket "always pin digests" rule:

| Stage | Tag style | Why |
|---|---|---|
| **Dev / local build** (Dockerfile base images) | **mutable minor tags** (`golang:1.25-alpine`, `postgres:18`) | Auto-pick patch fixes; nothing to babysit. The research's own "dev minor tags are safe" exception. |
| **Published release image** (the quack image itself) | **immutable tag/digest** | `cd.yaml` tags each release `1.2.3` / `1.2` / `latest` / `sha-<sha>` via metadata-action; consumers pull a fixed version or its `@sha256` digest. |

Quack's *published* image is the reproducibility unit - it's pinned at publish time, so the Dockerfile's **base** images stay on mutable minor tags by design (one less thing to bump). Pin the base images to digests too only if you need byte-reproducible rebuilds of a given release. The two `:latest` backends (`searxng`, `crawl4ai`) in compose are still a mild reproducibility hazard - pin those first if an overnight upstream change ever breaks a build. See `references/recipes.md` for the digest-resolution command.

## Quack conventions (match these)

- **3-stage SPA-embed flow**: stage 1 (`node`) builds the SPA → stage 2 (`golang`) copies that `dist` into `cmd/server/web/dist` and `go build` *embeds* it into the binary → stage 3 (distroless) gets just the binary plus the read-only `config/`, `agents/`, `skills/` dirs. Don't try to serve the SPA as separate static files; it lives inside the binary.
- **Build paths**: local dev/self-host via `make docker-up` (`docker compose up --build`); CI builds the image on every PR (`docker-build` job in `ci.yaml`, no push) so a broken Dockerfile fails the PR; `cd.yaml` builds + pushes to `ghcr.io/fagerbergj/quack` on a `v*.*.*` tag (with SBOM + provenance). Both CI/CD use the `type=gha` BuildKit cache.
- **Logs go to stdout** (see the `go-logging` skill) - let `docker compose logs` / the orchestration layer collect them; don't add log files or volumes for logs.
- **`config/`, `agents/`, `skills/` are read-only startup inputs**, copied into the image and never written at runtime. Adding a new such dir means a new `COPY` in the runtime stage *and* keeping it out of `.dockerignore`.

## Gotchas

- **Never `.dockerignore` `*.md` / `docs/` / `config/`.** The runtime stage copies `prompt.md`, `SKILL.md`, `rubric.md`, and `config/constitution.md` from the build context - ignoring markdown ships a silently broken image (agents/skills fail to load at startup). The `.dockerignore` carries a comment saying so; keep it.
- **Cache mounts are not committed to layers**, so they shrink build *time*, never final image *size*. Multi-stage `COPY --from` is what shrinks size. Don't conflate the two.
- **Distroless has no shell.** `RUN`, `sh`, and `docker exec … sh` don't exist in the runtime stage - do shell work in an earlier stage, and debug a running container with `docker debug` or a sidecar, not by adding a shell to the image.
- **`COPY . .` busts the build cache on any source change.** That's why dependency manifests are copied and installed *before* it. Don't move `COPY . .` above the dependency install.
- **Non-root can't bind ports <1024.** Quack uses 8080, so `:nonroot` is fine. If a service ever needs a privileged port, that's a config change (use a high port + publish-map), not a return to root.
- **`# syntax=docker/dockerfile:1` must stay** as line 1 - cache mounts and other BuildKit features parse only with it present.

## Validation Loop

After any change to these files:

1. `docker compose build` (or `make docker-up`) - succeeds.
2. Re-run the build - the dependency-install layers are cached and cache mounts hit (visibly faster); editing a `.go` file does **not** re-trigger `npm ci` / `go mod download`.
3. `docker compose up` → app reachable on `:8081`; a research query runs end-to-end (proves the `agents/`/`skills/`/`config/*.md` files made it into the image).
4. Image runs as non-root: `docker compose run --rm app id` (or `docker inspect`) shows UID 65532.
5. If you touched image size: eyeball `docker image inspect … --format '{{.Size}}'` against before.

## Resources

- Read `references/recipes.md` when writing or modifying a build stage, adding a cache mount, editing `.dockerignore`, or reaching for a **deferred** pattern (readiness healthchecks, `internal:` network split, `deploy.resources` limits, Compose `secrets:`, profiles, `x-*` anchors, digest pinning, registry cache, build secrets) - it has the copy-ready snippet for each, tagged with its trigger.
