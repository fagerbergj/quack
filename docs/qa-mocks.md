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
  --fixture ../../agent-researcher/testdata/qa/github/events/issues.labeled.quack-review.json \
  --event issues \
  --secret "$QUACK_QA_WEBHOOK_SECRET" \
  --url http://localhost:8080/api/v1/github/webhook
```

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
`testdata/qa/github/events/issues.labeled.quack-review.json`, default url
`http://localhost:8080/api/v1/github/webhook`). There's no chat id in the
webhook response or the mock's recorded delivery - quack replies 202 before
the run is created - so without `--chat` the script lists `/api/v1/chats`
and asks you to re-run with the new id. With `--chat`, it polls `quack api
/api/v1/chats/<id>` until the run leaves `running`, then dumps `/artifacts`
and the mock's `deliveries.jsonl`. Requires both mocks and a QA quack server
already up per the config above - it does not start them.
