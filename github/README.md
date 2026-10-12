# github

quack's GitHub App integration: webhook triggers, dispatch envelopes, staged delivery, CI auto-heal and merge.

## Plugin

`plugin/` is the code-agent plugin quack loads through its plugin registry: `plugin.json` and the
`code-implementer`, `code-reviewer` and `code-explorer` bundles under `agents/` (card, prompt, rubric,
memory, and `agent.yaml` with judge rounds, context window and model role). quack seeds it as
`github:fagerbergj/quack-extensions@github/vX.Y.Z#github/plugin`, a registry row named `github`, and
seeds its agents only while `extensions.github` is configured. The deployment's own `agents.<name>:`
override supplies the `acp:` block that makes each one an ACP agent.

The agents name no tools: their staging tools come from quack's own ACP MCP server, not this module.
So unlike sleeper, the plugin's seed ref need not match quack's `go.mod` pin of this module, and a
prompt or rubric change ships with a `github/vX.Y.Z` tag and a seed bump, without a quack release.
`plugin.json`'s `version` is that tag's version; bump it in the PR that precedes the tag.

`plugin_test.go` checks each card names its directory and pins the prompt and rubric contracts quack's
code relies on: staging over the structured tail, slice-scoped reviews staging no verdict, and the
reviewer rubric's severity/verdict coherence and code-block rules.

To change a prompt on a running deployment:

1. Merge the change and tag `github/vX.Y.Z`.
2. Bump the deployment's `plugins.seed` entry to that tag, then either restart (a seeded row whose
   seed entry changed moves to the new entry and is fetched at boot), or apply it live with
   `quack api POST /api/v1/plugins -d '{"entry":"github:fagerbergj/quack-extensions@github/vX.Y.Z#github/plugin"}'`,
   which fetches the tag and reloads. A REST-set row belongs to the operator until a boot finds it
   equal to its seed entry again, so edit the seed too. The first switch from a local
   `.agents/plugins/github` row needs the restart: REST answers 409 for a name held by a local row.

## Decision points

The extension declares five observe-only points (`DecisionPoints()` in `decisions.go`). quack namespaces each as `ext:github/<name>` and records the handler's answer beside the extension's own; no answer, error or timeout changes what the extension posts or dispatches. Every call runs in a goroutine off the posting or dispatch path, one at a time, each capped at 10s.

| Point | Primary question | Asked per | State | Baseline |
| --- | --- | --- | --- | --- |
| `finding.severity` | `severity` (blocking, suggestion, nit, question) | labelled review finding | path, line, the finding's text with every label and severity marker stripped, and the diff hunk it is anchored to | the finding's label |
| `finding.blocking` | `blocking` (noul) | review finding | as `finding.severity` | `true` when labelled blocking |
| `review.verdict` | `verdict` (approve, comment, request_changes) | posted review | PR title and body, every finding (path, line, label-stripped text), the changed files, and the hunks the findings are anchored to; no severities and no reviewer summary. Bounded to 9,500 runes: findings first, hunks get what is left, cuts marked `…[truncated]` | the staged verdict |
| `intent` | `write` (noul), plus `deliverable` (reply, review, commit, pull_request, plan) | free-text mention on an issue or PR | see below | `true` when the classifiers picked a commit or a new PR |
| `ci.flaky` | `flaky` (noul) | failing check, up to 5, on the auto-heal and `quack:fix` paths | see below | none |

`intent` is asked only for mentions; label triggers, slash commands and synthetic runs (CI fix, review follow-up) classify nothing. Its state is the comment body, the issue or PR title, the sender and their author association, and the grant (the delivery kinds labels and authorship allow). The baseline covers `write` only: the extension's deliverable pick is in that chat's envelope `<deliverable>` block, not in the decision record.

`ci.flaky` has no baseline at failure time. Its state carries `repo`, `pr`, `head_sha` and `check`, plus the check's summary and annotations and the PR's changed files (first 50). A scorer labels a decision by joining `(repo, head_sha, check)` to that check run's later conclusion on the same sha: a re-run that passes means flaky, a re-run that fails again means a real failure, and no re-run leaves it unlabelled. The check-runs API returns the latest run of that check on the sha by default; add `filter=all` to list every run (`GET /repos/{repo}/commits/{head_sha}/check-runs?check_name={check}&filter=all`). The extension never re-runs a check itself.

Each request names its PR or issue chat (`DecideRequest.ChatID`), so quack attributes the webhook-side asks, which have no run context, to that chat's ledger.
