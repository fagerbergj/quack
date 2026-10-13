# syntax=docker/dockerfile:1

# 1) Build the SPA (the committed src/generated client is used as-is).
# bookworm (glibc), NOT alpine (musl): the runtime stage copies node out of this
# stage, and a musl-linked node cannot run on the debian runtime base.
FROM node:24-bookworm-slim AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
# Cache mount: npm's download cache persists across builds, never lands in a layer.
RUN --mount=type=cache,target=/root/.npm npm ci
# Tests and stories never reach the bundle; CI's `npm run build` still type-checks them.
COPY --exclude=**/*.test.ts --exclude=**/*.test.tsx --exclude=**/*.stories.tsx frontend/ ./
RUN npm run build

# 1b) Mermaid validator deps (#574): mermaid.parse() run for real, not the Go
# reimplementation it replaces (internal/vetting/mermaid.go). npm install
# only - no bundler - the image already carries the Go toolchain, node, and
# pi, so ~180MB of node_modules here is not a meaningful addition.
FROM node:24-bookworm-slim AS mermaid-validator
WORKDIR /app/scripts
COPY scripts/package.json scripts/package-lock.json ./
# Source maps only feed stack-trace rewriting; dropping them saves ~70MB.
RUN --mount=type=cache,target=/root/.npm npm ci --omit=dev \
    && find node_modules -type f -name '*.map' -delete
COPY scripts/mermaid-validate.mjs ./

# 2) Build the Go server with the SPA embedded.
# bookworm (glibc), NOT alpine (musl) - same reason as the frontend stage: the
# runtime copies /usr/local/go from here and must be able to execute it.
FROM golang:1.26-bookworm AS backend
# The runtime ships this GOROOT. Only `go test std` and `go tool dist test` read
# these; src/cmd stays because go builds tools absent from pkg/tool on demand.
RUN cd /usr/local/go && rm -rf test api doc \
    && find src \( -name testdata -type d -prune -o -name '*_test.go' \) -exec rm -rf {} +
WORKDIR /app
COPY go.mod go.sum ./
# go.mod replaces the sdk module with this dir, so resolving the graph needs its go.mod.
COPY extensions/sdk/go.mod extensions/sdk/go.sum ./extensions/sdk/
# The runtime ships /go/pkg/mod (#936), so it is filled offline from the cache mount.
# Zips go: go build/test and go mod verify read the extracted tree plus .ziphash.
RUN --mount=type=cache,target=/root/gomod-dl \
    GOMODCACHE=/root/gomod-dl go mod download \
    && GOPROXY=file:///root/gomod-dl/cache/download go mod download \
    && find /go/pkg/mod/cache/download -name '*.zip' -delete
# Not build inputs, so editing them leaves the go build cached: the SPA comes
# from the frontend stage and the runtime COPYs pi-acp/config from the context.
# Extension plugin/ dirs are fetched at runtime from a plugin/* tag, never embedded.
COPY --exclude=docs --exclude=frontend --exclude=scripts --exclude=testdata --exclude=tools \
     --exclude=extensions/*/plugin --exclude=extensions/sleeper/ui/storybook \
     --exclude=Dockerfile --exclude=*.md --exclude=**/*_test.go --exclude=**/testdata . .
COPY --from=frontend /app/frontend/dist ./internal/serve/web/dist

# VERSION stamps main.version (cmd/quack/main.go); cd.yaml passes the release
# tag, defaults to "dev" so a plain `docker build` / `make build` still works.
ARG VERSION=dev
# -trimpath + -ldflags="-s -w": strip local paths and the symbol/DWARF tables
# (smaller, reproducible binary). Build cache is mounted (pure speed, discarded);
# /go/pkg/mod is not - see the go mod download comment above.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /quack ./cmd/quack
# Landlock self-exec target, built separately so a sandboxed child re-execs a
# few MB instead of the full server binary on every spawn.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quack-sandbox ./cmd/quack-sandbox
# Extensions as child processes, run when quack.yaml sets extensions.<name>.exec: /quack-ext-<name>.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o / ./extensions/usage/cmd/quack-ext-usage ./extensions/remarkable/cmd/quack-ext-remarkable \
        ./extensions/sleeper/cmd/quack-ext-sleeper

# 2b) The external ACP coding agent: pi, driven through the tools/pi-acp shim.
# PINNED - the RPC surface is integration-tested per release, so bump
# deliberately. ~145MB node_modules vs the 170MB single-binary agent it replaced
# (node was already in the image for mermaid/frontend).
FROM node:24-bookworm-slim AS pi
WORKDIR /opt/pi
# pi's shrinkwrap pulls esbuild binaries for ~25 platforms (~275MB); keep this
# one. Source maps only feed stack-trace rewriting.
RUN npm install --no-fund --no-audit @earendil-works/pi-coding-agent@1.0.0 \
    && p="$(node -p 'process.platform + "-" + process.arch')" \
    && find node_modules -path '*/@esbuild/*' -prune ! -name "$p" -exec rm -rf {} + \
    && find node_modules -type f -name '*.map' -delete

# 2c) The opt-in PIG engine (PI_ACP_ENGINE=pig): PIG with tools/pi-acp/pig/quackgate fused in, one static
# ~64MB Node-free binary. PINNED like pi: bump all three values together and rerun the shim's real-engine test.
FROM golang:1.26-bookworm AS piglet
# The release's SHA256SUMS entry for the linux-amd64 tarball (the image is amd64-only, see cd.yaml).
ADD --checksum=sha256:3b4de3587cbfb617def483ec62dddcf35827acc3436b1454cc5694c22dcdaf3f \
    https://github.com/MichaelKinsy/PiG/releases/download/v0.4.1/pig-0.4.1-linux-amd64.tar.gz /tmp/pig.tgz
WORKDIR /src
COPY tools/pi-acp/pig/ ./
# `piglet build` compiles PIG's own v0.4.1 module source, fetched through GOPROXY and checked by GOSUMDB;
# pinning its go.sum hash here also catches a retagged release. Network is needed only in this stage.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    tar -xzf /tmp/pig.tgz -C /tmp \
    && sum="$(go mod download -json github.com/MichaelKinsy/PiG@v0.4.1 | sed -n 's/^\t"Sum": "\(.*\)",$/\1/p')" \
    && test "$sum" = "h1:1W63pubkCe4ZuueDtsKkiISli4vqO2fNTEBuI9jVpdE=" \
    && /tmp/pig-0.4.1-linux-amd64/pig piglet build quack.yaml --format binary --out /pig-quack \
    && /pig-quack --version

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
      git ca-certificates bubblewrap util-linux make poppler-utils tini \
      python3 python3-venv python3-pip \
      gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
# Symlinks dangle until the COPYs land. No RUN follows the COPYs, and --link
# caches each COPY on its own content, so a change re-copies only its layer.
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && ln -s /usr/local/lib/pi/node_modules/.bin/pi /usr/local/bin/pi
ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /
# Build toolchains for the coding agents (#283): the code-implementer must be
# able to `go build/test` and `npx tsc`/`npm test` what it writes - without them
# every check fails with "not found" and the worker grinds hunting a toolchain
# that isn't there. Copied from the build stages, so runtime versions exactly
# match what CI builds with (~+500MB, a deliberate size tradeoff). Go's
# per-user caches (GOPATH/GOCACHE) default under $HOME, which quack points at
# the writable workspace volume - nothing writes outside it.
COPY --link --from=backend /usr/local/go /usr/local/go
# go.sum's module cache (#936), under GOROOT rather than $HOME: the sandbox
# ro-binds /usr whole but never /home, and workspace.env's GOMODCACHE default
# points here. Extracted read-only (0444/0555) by go mod download itself, so
# uid 65532 can read it with no chown - and Go still fails loudly, not
# silently online, on any module go.sum doesn't already cover.
COPY --link --from=backend /go/pkg/mod /usr/local/go/pkg/mod
COPY --link --from=frontend /usr/local/bin/node /usr/local/bin/node
COPY --link --from=frontend /usr/local/lib/node_modules /usr/local/lib/node_modules
# Under /usr (not /opt): the bwrap jail ro-binds /usr whole; /opt is invisible
# to worker subprocesses (EACCES at spawn).
# The ACP coding agent (agents.<name>.acp: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
# - resolved via the server's PATH). The shim writes pi's per-round config
# (models.json/settings.json/extensions) into TMPDIR; session state is off
# (--no-session).
COPY --link --from=pi /opt/pi/node_modules /usr/local/lib/pi/node_modules
COPY --link tools/pi-acp/pi-acp.mjs tools/pi-acp/mcp-client.mjs tools/pi-acp/otel.mjs /usr/local/lib/pi-acp/
# The shim's PI_ACP_ENGINE=pig default; under /usr so the jail sees it.
COPY --link --from=piglet /pig-quack /usr/local/bin/pig-quack
# The mermaid validator (internal/vetting/mermaid.go, #574): `node
# scripts/mermaid-validate.mjs` resolves against CWD / here, exactly like
# every path below - same relative path the source tree uses in dev.
COPY --link --from=mermaid-validator /app/scripts /scripts
# The config directory: quack.yaml plus files it references by relative path
# (CWD is /), e.g. the trust gate's rubric at config/constitution.md.
COPY --link config/ /config/
# Declarative agent bundles (agent-card.json + prompt.md), read at startup. The
# config references them by the relative path `agents/...` (CWD is /), so keep
# that layout.
COPY --link agents/ /agents/
# Orchestrator skill bundles (SKILL.md directories), read at startup.
COPY --link skills/ /skills/
# Last: they change on every code edit, so the layers above stay pullable from cache.
COPY --link --from=backend /quack-sandbox /quack-sandbox
COPY --link --from=backend /quack-ext-usage /quack-ext-usage
COPY --link --from=backend /quack-ext-remarkable /quack-ext-remarkable
COPY --link --from=backend /quack-ext-sleeper /quack-ext-sleeper
COPY --link --from=backend /quack /quack
ENV QUACK_CONFIG=/config/quack.yaml
USER nonroot
EXPOSE 8080
# tini reaps children orphaned onto PID 1 (pi, sh, git from agent sandboxes);
# a wait(-1) loop inside quack would race os/exec.Cmd.Wait for exit statuses.
ENTRYPOINT ["/usr/bin/tini", "--", "/quack"]
