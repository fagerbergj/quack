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
# opencode, so ~180MB of node_modules here is not a meaningful addition.
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
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./internal/serve/web/dist
# -trimpath + -ldflags="-s -w": strip local paths and the symbol/DWARF tables
# (smaller, reproducible binary). Module + build caches are mounted, not embedded.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /quack ./cmd/quack

# 2b) The external ACP coding agent (internal/acp, docs/acp-coder.md): one
# self-contained binary, extracted in its own stage so the runtime layer gets
# the binary without the tarball. PINNED - the ACP surface is integration-
# tested per release (internal/acp/live_test.go), so bump deliberately.
FROM debian:bookworm-slim AS opencode
ADD https://github.com/sst/opencode/releases/download/v1.18.3/opencode-linux-x64.tar.gz /tmp/opencode.tar.gz
RUN tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin opencode

# 2c) ast-grep (#684): a Rust single binary (MIT) the trust gate's
# package-declaration check (internal/vetting/packagecheck.go) shells out to,
# and that the coding agents' own bash access can already reach once it's on
# PATH. PINNED - bump deliberately. The release archive also ships `sg`, a
# short alias - never extracted: `sg` is already util-linux's setgid(1) on
# this image's PATH, and shadowing it would resolve to the wrong binary.
FROM debian:bookworm-slim AS ast-grep
RUN apt-get update && apt-get install -y --no-install-recommends unzip ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ADD https://github.com/ast-grep/ast-grep/releases/download/0.45.0/app-x86_64-unknown-linux-gnu.zip /tmp/ast-grep.zip
RUN unzip -o /tmp/ast-grep.zip ast-grep -d /usr/local/bin && chmod +x /usr/local/bin/ast-grep

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
# per-child rlimits (workspace.limits).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates bubblewrap util-linux make \
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
# The ACP coding agent (agents.<name>.acp: ["opencode", "acp"] - resolved via
# the server's PATH). Its per-round state/caches land under the subprocess
# $HOME quack sets (the jail home on the workspace volume), and its first round
# fetches the @ai-sdk provider package over the network into that cache.
COPY --from=opencode /usr/local/bin/opencode /usr/local/bin/opencode
COPY --from=ast-grep /usr/local/bin/ast-grep /usr/local/bin/ast-grep
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
# Vendored skill libraries (git submodules under .agents/vendor: dotagents -
# the general-purpose skills, incl. startup-required format-markdown - and
# ponytail), merged into the skill toolset via relative paths against CWD /
# (internal/serve's newSkillSource). dotagents is also embedded in the binary,
# so only ponytail is lost if a submodule isn't initialized in the build
# context - build with `git submodule update --init` to include everything.
COPY .agents/ /.agents/
ENV QUACK_CONFIG=/config/quack.yaml
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/quack"]
