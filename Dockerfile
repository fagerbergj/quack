# syntax=docker/dockerfile:1

# 1) Build the SPA (the committed src/generated client is used as-is).
# bookworm (glibc), NOT alpine (musl): the runtime stage copies node out of this
# stage, and a musl-linked node cannot run on the debian runtime base.
FROM node:24-bookworm-slim AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
# Cache mount: npm's download cache persists across builds, never lands in a layer.
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
RUN npm run build

# 1b) Mermaid validator deps (#574): mermaid.parse() run for real, not the Go
# reimplementation it replaces (internal/vetting/mermaid.go). npm install
# only - no bundler - the image already carries the Go toolchain, node, and
# pi, so ~180MB of node_modules here is not a meaningful addition.
FROM node:24-bookworm-slim AS mermaid-validator
WORKDIR /app/scripts
COPY scripts/package.json scripts/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --omit=dev
COPY scripts/mermaid-validate.mjs ./

# 2) Build the Go server with the SPA embedded.
# bookworm (glibc), NOT alpine (musl) - same reason as the frontend stage: the
# runtime copies /usr/local/go from here and must be able to execute it.
FROM golang:1.26-bookworm AS backend
WORKDIR /app
# VERSION stamps main.version (cmd/quack/main.go); cd.yaml passes the release
# tag, defaults to "dev" so a plain `docker build` / `make build` still works.
ARG VERSION=dev
COPY go.mod go.sum ./
# Not cache-mounted (#936): a cache mount's writes never land in an image layer,
# and the runtime stage needs this cache to survive into one - see GOMODCACHE below.
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./internal/serve/web/dist
# Not in git and go:embed needs them, so fetch before building.
RUN ./scripts/plugins.sh
# -trimpath + -ldflags="-s -w": strip local paths and the symbol/DWARF tables
# (smaller, reproducible binary). Build cache is mounted (pure speed, discarded);
# /go/pkg/mod is not - see the go mod download comment above.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /quack ./cmd/quack

# 2b) The external ACP coding agent: pi, driven through the tools/pi-acp shim.
# PINNED - the RPC surface is integration-tested per release, so bump
# deliberately. ~145MB node_modules vs the 170MB opencode binary it replaced
# (node was already in the image for mermaid/frontend).
FROM node:24-bookworm-slim AS pi
WORKDIR /opt/pi
RUN npm install --no-fund --no-audit @earendil-works/pi-coding-agent@0.84.2

# 3) Minimal runtime. The git tools (internal/tools/git.go) exec the real git
# binary, which dynamically links against libcurl/libssl/libpcre2/zlib - those
# don't exist in distroless/static (no libc at all) and hand-copying git's
# shared-library closure into distroless/base is fragile and unmaintainable
# (ponytail: wrap the OS's own package manager, don't reimplement it). So the
# runtime base is debian:bookworm-slim with git installed via apt, still
# non-root via an explicit UID (65532, matching the prior distroless:nonroot
# convention) - a deliberate size/attack-surface tradeoff for a real userland,
# not a downgrade in the properties that actually matter here (non-root,
# unprivileged port, no runtime writes outside the workspace volume).
#
# bubblewrap is the OS boundary agent child processes (run_command, the trust
# gate's derived checks) run inside - workspace.sandbox: bwrap, the default, and
# the server REFUSES TO START without it. It needs no root and no daemon, but it
# does need the container runtime to permit unprivileged user namespaces (Docker's
# default seccomp profile does; a hardened runtime may not - see
# docs/configuration.md). util-linux carries prlimit(1), which applies the
# per-child rlimits (workspace.limits). poppler-utils carries pdftoppm/pdfinfo,
# used to render PDF attachments to images for vision models (#829).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates bubblewrap util-linux make poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
WORKDIR /
COPY --from=backend /quack /quack
# Build toolchains for the coding agents (#283): the code-implementer must be
# able to `go build/test` and `npx tsc`/`npm test` what it writes - without them
# every check fails with "not found" and the worker grinds hunting a toolchain
# that isn't there. Copied from the build stages, so runtime versions exactly
# match what CI builds with (~+500MB, a deliberate size tradeoff). Go's
# per-user caches (GOPATH/GOCACHE) default under $HOME, which quack points at
# the writable workspace volume - nothing writes outside it.
COPY --from=backend /usr/local/go /usr/local/go
# go.sum's module cache (#936), under GOROOT rather than $HOME: the sandbox
# ro-binds /usr whole but never /home, and workspace.env's GOMODCACHE default
# points here. Extracted read-only (0444/0555) by go mod download itself, so
# uid 65532 can read it with no chown - and Go still fails loudly, not
# silently online, on any module go.sum doesn't already cover.
COPY --from=backend /go/pkg/mod /usr/local/go/pkg/mod
# Under /usr (not /opt): the bwrap jail ro-binds /usr whole; /opt is invisible
# to worker subprocesses (EACCES at spawn).
# The ACP coding agent (agents.<name>.acp: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
# - resolved via the server's PATH). The shim writes pi's per-round config
# (models.json/settings.json/extensions) into TMPDIR; session state is off
# (--no-session).
COPY --from=pi /opt/pi/node_modules /usr/local/lib/pi/node_modules
RUN ln -s /usr/local/lib/pi/node_modules/.bin/pi /usr/local/bin/pi
COPY tools/pi-acp/pi-acp.mjs tools/pi-acp/mcp-client.mjs tools/pi-acp/otel.mjs /usr/local/lib/pi-acp/
COPY --from=frontend /usr/local/bin/node /usr/local/bin/node
COPY --from=frontend /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
ENV PATH=/usr/local/go/bin:$PATH
# The mermaid validator (internal/vetting/mermaid.go, #574): `node
# scripts/mermaid-validate.mjs` resolves against CWD / here, exactly like
# every path below - same relative path the source tree uses in dev.
COPY --from=mermaid-validator /app/scripts /scripts
# The config directory: quack.yaml plus files it references by relative path
# (CWD is /), e.g. the trust gate's rubric at config/constitution.md.
COPY config/ /config/
# Declarative agent bundles (agent-card.json + prompt.md), read at startup. The
# config references them by the relative path `agents/...` (CWD is /), so keep
# that layout.
COPY agents/ /agents/
# Orchestrator skill bundles (SKILL.md directories), read at startup.
COPY skills/ /skills/
# Agent Plugins roots, resolved against CWD / (config's plugins:): fetched
# trees under .agents/vendor, first-party manifests under .agents/plugins.
# From the builder: the context has no vendor trees.
COPY --from=backend /app/.agents/ /.agents/
# The startup refresh re-runs this against the pins in .agents/vendor.
COPY scripts/plugins.sh /scripts/plugins.sh
ENV QUACK_CONFIG=/config/quack.yaml
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/quack"]
