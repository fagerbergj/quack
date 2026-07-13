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

# 3) Minimal runtime. The git tools (internal/tools/git.go) exec the real git
# binary, which dynamically links against libcurl/libssl/libpcre2/zlib — those
# don't exist in distroless/static (no libc at all) and hand-copying git's
# shared-library closure into distroless/base is fragile and unmaintainable
# (ponytail: wrap the OS's own package manager, don't reimplement it). So the
# runtime base is debian:bookworm-slim with git installed via apt, still
# non-root via an explicit UID (65532, matching the prior distroless:nonroot
# convention) — a deliberate size/attack-surface tradeoff for a real userland,
# not a downgrade in the properties that actually matter here (non-root,
# unprivileged port, no runtime writes outside the workspace volume).
#
# bubblewrap is the OS boundary agent child processes (run_command, the trust
# gate's derived checks) run inside — workspace.sandbox: bwrap, the default, and
# the server REFUSES TO START without it. It needs no root and no daemon, but it
# does need the container runtime to permit unprivileged user namespaces (Docker's
# default seccomp profile does; a hardened runtime may not — see
# docs/configuration.md). util-linux carries prlimit(1), which applies the
# per-child rlimits (workspace.limits).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates bubblewrap util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
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
# Vendored skill libraries (git submodule at .agents/vendor/ponytail — the
# code-implementer's coding-discipline skills), merged into the skill toolset
# when present (internal/serve's newSkillSource resolves the relative path
# .agents/vendor/ponytail/skills against CWD /). Copying all of .agents/ keeps
# this COPY valid even when the submodule isn't initialized in the build
# context (the dir then just lacks vendor/ and the server runs without the
# vendored skills — build with `git submodule update --init` to include them).
COPY .agents/ /.agents/
ENV QUACK_CONFIG=/config/quack.yaml
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/quack"]
