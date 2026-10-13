# Sleeper extension

The `sleeper` extension (`extensions/sleeper/`) gives agents read-only `sleeper_*` tools over the [Sleeper](https://sleeper.com) fantasy-football API, serves a page at `/sleeper/` that launches weekly jobs and renders their artifacts, takes optional daily league snapshots, and declares observe-only decision points. Its design, routes and tools are in the [extension's README](../../extensions/sleeper/README.md); its decision points are in [decisions.md](../configuration/decisions.md).

```yaml
extensions:
  sleeper:
    default_user: "860211057418018816"           # Sleeper username or user_id tools fall back to
    default_league: "1356407594683482112"        # lets chat and the page omit league_id
    snapshots: daily                             # off (default) or daily; daily needs default_league
    timezone: America/Chicago                    # optional; overrides quack's zone for *_local fields
    base_url: http://sleeper-mock:8092           # optional; defaults to https://api.sleeper.app
```

`enabled`/`data_dir`/`exec` are the SDK-level keys every extension block accepts (see the [extensions README](../../extensions/README.md)). `base_url` points the client at another API host, such as `extensions/sleeper/cmd/qa-mock` replaying its recorded fixtures.

The image ships this extension as a child-process binary too: `exec: /quack-ext-sleeper` runs it out of process. Its tools, page and JSON routes, job dispatch, RunEnded, artifact reads for the page and the decision questions, artifact schemas, decision points and daily snapshots all behave as they do compiled in, and each `sleeper_*` tool call adds one stdio round trip. A child restart drops what the extension keeps in memory while quack's runs continue: which jobs show Running, each card's unverified note, and a pending season-notes redispatch.

The agents, skills and workflow shapes ship in the sleeper plugin (`extensions/sleeper/plugin`, seeded as `github:fagerbergj/quack@plugin/sleeper/vX.Y.Z#extensions/sleeper/plugin` while `extensions.sleeper` is configured), the same whether the extension runs in or out of process.
