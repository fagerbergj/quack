# Usage dashboard extension

The `usage` extension (`extensions/usage/`) embeds an in-app usage dashboard in the SPA, backed by a Prometheus it reads server-side. Inbound-only and no-dispatch: it serves the dashboard page and a narrow Prometheus query proxy behind quack's session auth, nothing else.

```yaml
extensions:
  usage:
    prometheus_url: http://prometheus:9090   # required
    tempo_url: http://tempo:3200             # optional - surfaced to the page for a future trace-drilldown link; never queried today
    default_range: 24h                       # Go duration string; initial time range, default 24h
```

`enabled`/`data_dir`/`exec` are the SDK-level keys every extension block accepts (see the [extensions README](../../extensions/README.md)). The image ships this extension as a child-process binary too: `exec: /quack-ext-usage` runs it out of process with the same routes and page.

The page's HTML/JS ships from the extension's own embedded assets and links the [extension UI kit](ui-kit.md) so it looks native to the SPA.

## Outcome trends

Four plain time series below the cost panels show whether the work got cheaper and cleaner. They draw no targets: each one divides counters within a step.

- **Cache hit rate by role**: cached ÷ (cached + input) prompt tokens, split by `role`. A judge call counts under `judge` and bills the agent it judged; `orchestrator` is any call outside a DAG node.
- **First-round judge pass rate**: first-round passes ÷ first rounds from `quack.judge.verdict{first_round="true"}`; the revision rate is the rest.
- **Rejected tool calls by tool**: `quack.tool.calls{rejected="true"}`, calls that returned an error to the model and cost it a recovery request.
- **Cost per task**: `gen_ai.client.cost` ÷ `quack.run.duration`'s count, where a task is one orchestrator run.

No series carries a chat, turn or node id, so per-task figures are window averages, not per-chat drill-downs.
