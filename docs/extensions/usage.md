# Usage dashboard extension

The `usage` extension (in [quack-extensions](https://github.com/fagerbergj/quack-extensions), `usage/`) embeds an in-app usage dashboard in the SPA, backed by a Prometheus it reads server-side. Inbound-only and no-dispatch: it serves the dashboard page and a narrow Prometheus query proxy behind quack's session auth, nothing else.

```yaml
extensions:
  usage:
    prometheus_url: http://prometheus:9090   # required
    tempo_url: http://tempo:3200             # optional - surfaced to the page for a future trace-drilldown link; never queried today
    default_range: 24h                       # Go duration string; initial time range, default 24h
```

`enabled`/`data_dir` are the SDK-level keys every extension block accepts (see the [quack-extensions README](https://github.com/fagerbergj/quack-extensions)).

The page's HTML/JS ships from the extension's own embedded assets and links the [extension UI kit](ui-kit.md) so it looks native to the SPA.
