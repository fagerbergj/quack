# Deployment shapes

`server.topology` (see [index.md](index.md)) is the one knob that picks how far you scale: the same config shape works whether the stores are none, containerized, or already running elsewhere. Three full worked examples live in [`examples/`](examples/) — each is a complete, runnable `quack.yaml`, not a snippet.

## 1. Fully local, no containers

[`examples/local-cli.yaml`](examples/local-cli.yaml) — the fastest way in, see the [Quickstart](../../README.md#quickstart). `server.topology: embedded` uses sqlite for the relational store and there's no qdrant store at all, so memory stays disabled and nothing needs a container. One agent (`web-researcher`) plus the `synthesizer`, keyless web tools, no coding agents.

`quack init` writes something close to this when you pick "Local" and leave the optional features off. Good for trying quack out or single-user local use; no Postgres, no qdrant, no Docker.

```bash
./quack server run --config docs/configuration/examples/local-cli.yaml
```

## 2. Local, full stack via Docker

[`examples/docker-compose.yaml`](examples/docker-compose.yaml) — shown with its values resolved (the actual shipped `config/quack.yaml` keeps every one as an `${ENV_VAR}`; `docker-compose.yml`'s `environment:` block is what fills them in). All seven default agents, Postgres + qdrant + SearXNG + crawl4ai, `server.topology: external` (it points at the compose-provided stores rather than managing them itself).

```bash
cp .env.example .env   # set QUACK_LLM_ENDPOINT to something reachable from the container
docker compose up --build
```

Open `http://localhost:8081` - the app is remapped, since host port 8080 is taken by SearXNG in this stack.

This is the middle ground: full feature set, memory included, still a single `docker compose up`.

`server.topology: managed` is the same idea without `docker-compose.yml`: `quack server run --config config/managed.yaml` brings up just the Postgres + qdrant containers itself (an embedded compose file, project `quack-stores`, written to `~/.quack/stores.compose.yml`) and re-runs `up` idempotently on every start. There is no automatic tear-down: the stack keeps running after the server stops, so take it down with `docker compose -p quack-stores -f ~/.quack/stores.compose.yml down`. Reach for it if you want the containerized stores without hand-rolling compose.

On SIGTERM the server pauses every running node at once, cutting off its current step but keeping its session, and waits up to `server.shutdown_grace_seconds` (default 20) for the runs to wind down before force-cancelling the rest. The next boot continues each paused node where it was cut, and an extension hears how its run ended only once the resumed run finishes. A turn cut while the orchestrator was still planning has no node to continue, so the next boot marks it failed (resend to retry) and tells its extension the run failed. A cut run never streams `done`, so an open chat page keeps reconnecting for about three minutes and follows the resumed run once the new process is up.

## 3. Remote server, full-featured

[`examples/remote-full.yaml`](examples/remote-full.yaml) — the same shape as the repo's own `config/quack.yaml`, with the pieces that ship commented out there turned on: the [GitHub App extension](../extensions/github.md) and OTel actually exporting somewhere. This is the shape for a shared, always-on deployment: `server.topology: external`, pointing at Postgres/qdrant instances that outlive any one `quack` process, the judge always on. See [`docs/configuration/`](index.md) for what every section does.

## Deploying the UI separately

By default the SPA ships inside the quack binary, so every UI change rides a backend release. `ghcr.io/fagerbergj/quack-ui:<tag>` (built from `frontend/Dockerfile`, published on the same tags as `quack`) serves the SPA from nginx on port 8080 and proxies `/api/`, `/health` and the extension prefixes to quack, so the browser still sees one origin. Set `server.ui: none` on quack so it serves only the API, and route your edge proxy to `quack-ui` instead of quack:

```yaml
services:
  quack:
    image: ghcr.io/fagerbergj/quack:0.90.0
    volumes:
      - ./quack.yaml:/config/quack.yaml:ro   # server: { ui: none }
  quack-ui:
    image: ghcr.io/fagerbergj/quack-ui:0.90.0
    environment:
      QUACK_UPSTREAM: http://quack:8080              # no path, no trailing slash
      QUACK_EXTENSION_PREFIXES: usage|sleeper|github # every enabled extension name, |-separated
    ports:
      - "8080:8080"
    depends_on: [quack]
    restart: unless-stopped
```

`QUACK_EXTENSION_PREFIXES` must list every extension enabled in `quack.yaml`, since each mounts at `/<name>/`; a missing one serves the SPA instead of the extension page or webhook. nginx resolves `QUACK_UPSTREAM` per request through the container's DNS, so recreating the quack container needs no `quack-ui` restart. Chat streams are proxied unbuffered with a one-hour read timeout. Hashed `/assets/` files are cached for a year; `index.html` and `version.json` revalidate on every load.

**Rollout.** Deploy the backend first, then the UI. Keep the backend's `x-api-min-version` (openapi.yaml) at or below the `x-api-version` of the live UI, or open tabs hit the blocking reload overlay with no newer UI to reload into. An open tab compares its own build id with `/version.json` on load, on stream reconnect and when it becomes visible, and offers a reload once a new UI is served. A UI built against a newer API than the server shows a notice without a reload button, since reloading would fetch the same UI.

**Rollback.** Pin the previous `quack-ui` tag and recreate the container. When rolling back the backend past an `x-api-version` bump, roll the UI back to a matching tag too; otherwise tabs show the older-server notice and features that need the newer API fail.
