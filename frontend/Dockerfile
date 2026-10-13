# syntax=docker/dockerfile:1

# quack-ui: the SPA on nginx, proxying API, health and extension paths to a quack running with server.ui: none.
# Build from the repo root so the root .dockerignore applies: docker build -f frontend/Dockerfile .
FROM node:24-bookworm-slim AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY --exclude=**/*.test.ts --exclude=**/*.test.tsx --exclude=**/*.stories.tsx frontend/ ./
# .gz siblings for gzip_static, so nginx never compresses on the fly.
RUN npm run build \
    && find dist -type f \( -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.svg' -o -name '*.json' \) \
       -exec gzip -9 -k {} +

FROM nginxinc/nginx-unprivileged:1.30-alpine
ENV QUACK_UPSTREAM=http://quack:8080 \
    QUACK_EXTENSION_PREFIXES="usage|sleeper|remarkable|github" \
    NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1 \
    NGINX_ENVSUBST_FILTER="^(QUACK_|NGINX_LOCAL_RESOLVERS)"
COPY frontend/nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
