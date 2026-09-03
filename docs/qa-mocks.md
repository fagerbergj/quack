# QA mocks: GitHub and reMarkable without credentials

Exercises the `quack:review`/`plan`/`implement`/`fix` and reMarkable document
flows against a QA server with no real GitHub App, no public webhook, and no
reMarkable/rmfakecloud account. Both mocks are standalone Go tools in
`quack-extensions` (branch `qa/github-remarkable-mocks`), not part of the
`quack` binary - anything that talks to the running server itself goes
through `quack api`, per [`docs/cli.md`](cli.md).

## GitHub mock

```bash
cd quack-extensions/github
go run ./cmd/qa-mock serve --fixtures ../../agent-researcher/testdata/qa/github --addr :8090
```

The GitHub fixtures live in core, at `testdata/qa/github` (this repo) - every
`--fixtures`/`--fixture` path below is relative to `quack-extensions/github`,
hence the `../../agent-researcher/...` prefix. There are no reMarkable fixture files in this PR; the
`--fixtures ../../agent-researcher/testdata/qa/remarkable` path below is
just where `serve`/`drop` will persist `docs.json` and dropped PDFs the
first time you run them - create the directory or let `drop` create it.

Point the QA server's `quack.yaml` at it:

```yaml
extensions:
  github:
    client_id: Iv23liQAExample
    private_key_path: /run/secrets/qa-throwaway.pem   # any RSA key - never verified against real GitHub
    webhook_secret: ${QUACK_QA_WEBHOOK_SECRET}
    api_base: http://localhost:8090                    # the QA-only lever - see App.SetAPIBase
    allowed_users: [fagerbergj]
    triggers: [label, issue_plan, issue_implement, merge, ci_fix]
    labels: { review: "quack:review" }                  # the fixture below fires this label
```

Fire a webhook at the running quack server:

```bash
go run ./cmd/qa-mock send \
  --fixture ../../agent-researcher/testdata/qa/github/events/pull_request.labeled.quack-review.json \
  --event pull_request \
  --secret "$QUACK_QA_WEBHOOK_SECRET" \
  --url http://localhost:8080/github/webhook
```

`quack:review` only fires from a `pull_request` "labeled" event
(`handlePullRequest`) - an `issues` "labeled" event only drives the
`issue_plan`/`issue_implement` triggers, never review, no matter what label
name it carries. The webhook is mounted at `/<extension-name>/webhook`
(`/github/webhook` here), not under `/api/v1/` - verified against a live
server; `docs/extensions/github.md`'s `/api/v1/github/webhook` is wrong and
tracked separately.

Check what quack tried to post back to GitHub:

```bash
go run ./cmd/qa-mock deliveries --fixtures ../../agent-researcher/testdata/qa/github
```

GET fixtures live at `testdata/qa/github/get/<hash>.json`, keyed by
`sha256(METHOD_PATH?QUERY)[:16 hex]` - a miss 404s with a hint instead of a
made-up shape. To capture a new one from real GitHub once (e.g. a real PR's
`files`/`commits`), run `serve --record <a real installation token>` and hit
the mock the same way the extension would; the response is saved and every
run after that is offline and credential-free.

## reMarkable mock

```bash
cd quack-extensions/remarkable
go run ./cmd/qa-mock serve --fixtures ../../agent-researcher/testdata/qa/remarkable --addr :8091 \
  --email qa@example.com --password qa-password
```

```yaml
extensions:
  remarkable:
    base_url: http://localhost:8091
    email: qa@example.com
    password: qa-password
```

Simulate a new handwritten note landing:

```bash
go run ./cmd/qa-mock drop --fixtures ../../agent-researcher/testdata/qa/remarkable \
  --name "2-page note" --folder inbox --pdf /path/to/any.pdf
```

The extension's next poll picks it up like a real sync.

## End-to-end review script

```bash
scripts/qa/e2e-review.sh --secret "$QUACK_QA_WEBHOOK_SECRET" [--url URL] [--fixture FILE] [--chat ID]
```

Sends the fixture `quack:review` webhook (default fixture
`testdata/qa/github/events/pull_request.labeled.quack-review.json`, default
url `http://localhost:8080/github/webhook`). There's no chat id in the
webhook response or the mock's recorded delivery - quack replies 202 before
the run is created - so without `--chat` the script lists `/api/v1/chats`
and asks you to re-run with the new id. With `--chat`, it polls `quack api
/api/v1/chats/<id>` until the run leaves `running`, then dumps `/artifacts`
and the mock's `deliveries.jsonl`. Requires both mocks and a QA quack server
already up per the config above - it does not start them.

**Live-verified 2026-09-03** against a QA server built from this branch +
main (github v0.9.0 not yet cut, so the build used a local `replace` to this
branch's checkout - a real deploy needs that tag first): webhook accepted
(202), `github run dispatched`, a real chat created
(`ext:github:github-fagerbergj-quack-qa-1`), a real `git clone`+checkout of
the fixture's `clone_url`/head SHA, a code-implementer ACP round, two judge
rounds, and a revision, ending `idle` with `dag_plan`/`judge_round`/`text`
artifacts recorded. The three checked-in GET fixtures only cover the
`issues/1` meta call; `pulls/1` needed a fourth (`get/062e6861d63c49a7.json`,
added here) and `pulls/1/{files,commits,comments,reviews}`,
`issues/1/comments`, `commits/<sha>/check-runs`, and `GET /app` still 404 -
harmless, since every one of those fetches is best-effort and the extension
degrades gracefully instead of aborting, but a from-scratch `--record` pass
against a real PR is worth doing before relying on this fixture set for
anything beyond the review-only path exercised here.
