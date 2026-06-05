# syntax=docker/dockerfile:1

# 1) Build the SPA (the committed src/generated client is used as-is).
FROM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# 2) Build the Go server with the SPA embedded.
FROM golang:1.25-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./cmd/server/web/dist
RUN CGO_ENABLED=0 go build -o /quack ./cmd/server

# 3) Minimal runtime.
FROM gcr.io/distroless/static-debian12
WORKDIR /
COPY --from=backend /quack /quack
COPY config/quack.yaml /config/quack.yaml
# Declarative agent bundles (agent-card.json + prompt.md), read at startup. The
# config references them by the relative path `agents/...` (CWD is /), so keep
# that layout.
COPY agents/ /agents/
ENV QUACK_CONFIG=/config/quack.yaml
EXPOSE 8080
ENTRYPOINT ["/quack"]
