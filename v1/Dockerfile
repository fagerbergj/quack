# The v1 image: one build serves the distributor (with its session processes), the GitHub, schedules, Sleeper and reMarkable services; compose picks
# the entrypoint. Session processes review code, so the image carries git, Go and the language servers they use.
FROM golang:1.27.3-trixie AS gopls
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOBIN=/out go install golang.org/x/tools/gopls@v0.24.0

FROM node:26.13-trixie-slim AS deps
WORKDIR /app
COPY package.json package-lock.json ./
COPY packages/distributor/package.json packages/distributor/
COPY packages/github/package.json packages/github/
COPY packages/judge/package.json packages/judge/
COPY packages/research/package.json packages/research/
COPY packages/runtime/package.json packages/runtime/
COPY packages/remarkable/package.json packages/remarkable/
COPY packages/schedules/package.json packages/schedules/
COPY packages/sleeper/package.json packages/sleeper/
COPY packages/sdk/package.json packages/sdk/
COPY packages/session/package.json packages/session/
COPY packages/web/package.json packages/web/
COPY packages/workflows/package.json packages/workflows/
# typescript and typescript-language-server are dev dependencies the sessions run, so nothing is omitted.
RUN --mount=type=cache,target=/root/.npm npm ci

FROM deps AS web
COPY packages/web packages/web
RUN npm run build -w packages/web

FROM node:26.13-trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates python3 python3-venv && rm -rf /var/lib/apt/lists/*
# rmc draws reMarkable notebooks (packages/remarkable); its venv comes first on PATH so python3 finds it.
COPY packages/remarkable/requirements.txt /tmp/rm-requirements.txt
RUN python3 -m venv /opt/rmc && /opt/rmc/bin/pip install --no-cache-dir --no-deps -r /tmp/rm-requirements.txt && rm /tmp/rm-requirements.txt
COPY --from=golang:1.27.3-trixie /usr/local/go /usr/local/go
COPY --from=gopls /out/gopls /usr/local/bin/gopls
ENV PATH=/opt/rmc/bin:/usr/local/go/bin:/app/node_modules/.bin:$PATH
WORKDIR /app
COPY --from=deps /app/node_modules node_modules
COPY . .
COPY --from=web /app/packages/web/dist /app/web
ENV QUACK_WEB_DIR=/app/web
USER node
