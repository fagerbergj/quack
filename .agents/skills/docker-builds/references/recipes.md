# Docker Recipes (quack)

Concrete code for the decisions in `SKILL.md`. Copy and adapt; don't re-derive.

## 1. Multi-stage Dockerfile with cache mounts (the live shape)

```dockerfile
# syntax=docker/dockerfile:1   <- line 1, required for --mount

# 1) SPA. Manifest + install before the source copy, so editing source skips npm ci.
FROM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
RUN npm run build

# 2) Go server with the SPA embedded.
FROM golang:1.25-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./cmd/server/web/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quack ./cmd/server

# 3) Distroless, non-root. No shell, no package manager, UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=backend /quack /quack
COPY config/ /config/      # includes constitution.md — NOT ignorable
COPY agents/ /agents/      # includes prompt.md / agent-card.json per bundle
COPY skills/ /skills/      # includes SKILL.md per bundle
ENV QUACK_CONFIG=/config/quack.yaml
EXPOSE 8080
ENTRYPOINT ["/quack"]
```

Build flags: `-s` drops the symbol table, `-w` the DWARF debug info, `-trimpath` strips local filesystem paths from the binary (smaller + reproducible). `CGO_ENABLED=0` makes it static, which is what allows the `static-debian12` base.

## 2. Safe `.dockerignore`

```text
# Do NOT add *.md / docs/ / config/ — the runtime stage COPYs prompt.md, SKILL.md,
# rubric.md, and config/constitution.md from the context.
.git
.github
.vscode
.idea
**/node_modules
frontend/dist
cmd/server/web/dist/assets
/quack
*.test
*.out
.env
*.log
.DS_Store
Thumbs.db
```

## 3. Compose: restart policy (the one minimal addition)

```yaml
services:
  db:
    image: postgres:18
    restart: unless-stopped   # survive crashes/host reboots; not `always` (respects manual stop)
```

Applied to every long-running service (`db`, `searxng`, `crawl4ai`, `app`).

---

## Deferred compose patterns — add when the trigger fires

These are intentionally NOT in quack's compose today (single-host dev stack). Each block notes when it earns its place.

### Readiness gating — *add when first requests flakily fail because a backend isn't ready*

`depends_on: condition: service_started` only waits for the container to *start*. For real readiness, give the dependency a healthcheck and depend on `service_healthy`:

```yaml
  crawl4ai:
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:11235/health"]  # verify the path + that curl exists in the image
      interval: 10s
      timeout: 3s
      retries: 5
      start_period: 30s    # grace while Playwright warms up
  app:
    depends_on:
      crawl4ai:
        condition: service_healthy
```

### Network segmentation — *add when an untrusted service shares the network with the DB*

```yaml
  app:
    networks: [frontend, backend]
  db:
    networks: [backend]
networks:
  frontend:
  backend:
    internal: true   # no outbound internet for anything on this network
```

### Resource limits — *add when a container starves the host*

```yaml
  app:
    deploy:
      resources:
        limits: { cpus: "1.0", memory: 512M }
        reservations: { cpus: "0.25", memory: 128M }
```

### Compose secrets — *add when a real secret must not sit in the env/compose file*

The local Postgres password is `quack/quack` by design (throwaway). For a real secret:

```yaml
  app:
    secrets: [db_password]        # mounted read-only at /run/secrets/db_password
secrets:
  db_password:
    file: ./secrets/db.pass       # or: external: true
```

### Profiles — *add when some services should be opt-in* (`docker compose --profile monitoring up`)

```yaml
  prometheus:
    profiles: ["monitoring"]
```

### `x-*` anchors — *add when ≥3 services share a config block worth DRYing*

```yaml
x-app-defaults: &app-defaults
  restart: unless-stopped
services:
  web: { <<: *app-defaults }
  api: { <<: *app-defaults }
```

---

## 4. Digest pinning — *add when an image is published, or to freeze a flaky `:latest`*

Resolve the current digest, then pin it:

```console
$ docker buildx imagetools inspect searxng/searxng:latest --format '{{.Manifest.Digest}}'
sha256:abc123…
```

```dockerfile
FROM searxng/searxng:latest@sha256:abc123…
```

## 5. CI/CD build (live) — `.github/workflows/ci.yaml` + `cd.yaml`

CI builds the image on every PR (no push); CD publishes to GHCR on a `v*.*.*` tag. Both use the GitHub Actions BuildKit cache (`type=gha`), which is wiped-per-run-proof and zero-config:

```yaml
# ci.yaml — validate the build, don't push
- uses: docker/setup-buildx-action@v3
- uses: docker/build-push-action@v6
  with: { context: ., push: false, cache-from: type=gha, cache-to: "type=gha,mode=max" }

# cd.yaml — tag-derived publish with attestations
- uses: docker/metadata-action@v5
  id: meta
  with:
    images: ghcr.io/${{ github.repository }}
    tags: |
      type=semver,pattern={{version}}
      type=semver,pattern={{major}}.{{minor}}
      type=sha
- uses: docker/build-push-action@v6
  with:
    context: .
    push: true
    tags: ${{ steps.meta.outputs.tags }}
    labels: ${{ steps.meta.outputs.labels }}
    provenance: mode=max
    sbom: true
    cache-from: type=gha
    cache-to: type=gha,mode=max
```

Cut a release: `git tag v1.2.3 && git push origin v1.2.3`. `latest` moves only on tags, never on a push to main. Next steps if needed: a Trivy scan job (`aquasecurity/trivy-action`, upload SARIF) and cosign keyless signing (`sigstore/cosign-installer` + `id-token: write`).

For a non-GitHub runner, `type=registry` cache is the portable equivalent: `--cache-to type=registry,ref=<reg>/quack-cache:build --cache-from type=registry,ref=<reg>/quack-cache:build`.

## 6. Build secrets — *add when a build step needs a credential*

Never `ARG SECRET` (it lands in image history). Mount it instead — not committed, not cached:

```dockerfile
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc npm ci
```

```console
docker build --secret id=npmrc,src=$HOME/.npmrc .
```
