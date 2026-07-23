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

This is the middle ground: full feature set, memory included, still a single `docker compose up`.

`server.topology: managed` is the same idea without `docker-compose.yml`: `quack server run` brings up just the Postgres + qdrant containers itself (an embedded compose file) and tears them down on `quack server stop`. Reach for it if you want the containerized stores without hand-rolling compose.

## 3. Remote server, full-featured

[`examples/remote-full.yaml`](examples/remote-full.yaml) — the same shape as the repo's own `config/quack.yaml`, with the pieces that ship commented out there turned on: the [GitHub App extension](../extensions/github.md) and OTel actually exporting somewhere. This is the shape for a shared, always-on deployment: `server.topology: external`, pointing at Postgres/qdrant instances that outlive any one `quack` process, the judge always on. See [`docs/configuration/`](index.md) for what every section does.
