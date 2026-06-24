# syntax=docker/dockerfile:1

# 1) Build the SPA (the committed src/generated client is used as-is).
FROM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
# Cache mount: npm's download cache persists across builds, never lands in a layer.
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
RUN npm run build

# 2) Build the Go server with the SPA embedded.
FROM golang:1.25-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./internal/serve/web/dist
# -trimpath + -ldflags="-s -w": strip local paths and the symbol/DWARF tables
# (smaller, reproducible binary). Module + build caches are mounted, not embedded.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quack ./cmd/quack

# 3) Minimal runtime. The :nonroot variant runs as UID 65532 — safe here because
# the server makes no runtime filesystem writes (all state goes to Postgres) and
# binds the unprivileged port 8080. ponytail: mutable tag is fine for a
# locally/self-hosted build; pin to a digest only once a release image is published.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=backend /quack /quack
# The config directory: quack.yaml plus files it references by relative path
# (CWD is /), e.g. the trust gate's rubric at config/constitution.md.
COPY config/ /config/
# Declarative agent bundles (agent-card.json + prompt.md), read at startup. The
# config references them by the relative path `agents/...` (CWD is /), so keep
# that layout.
COPY agents/ /agents/
# Orchestrator skill bundles (SKILL.md directories), read at startup.
COPY skills/ /skills/
ENV QUACK_CONFIG=/config/quack.yaml
EXPOSE 8080
ENTRYPOINT ["/quack"]
