# GitHub extension

Quack can run as a **GitHub App**. Installed on a repo, it turns labels and `/quack` comments into orchestrator runs, and replies on the issue or PR - posting plans, opening PRs, and leaving reviews as itself.

The whole loop, driven by a label:

```text
someone applies quack:implement to an approved issue
  → quack clones the repo, writes the code, commits locally
  → its trust gate vets the diff, then opens a PR (pre-labeled for review)
  → applying quack-auto-review (or opening a PR with pr_opened enabled) triggers a review
  → quack posts one review with inline comments and a verdict
  → a maintainer applies quack:merge - quack squash-merges only if its own review approved
```

or by mentioning it in a comment:

```text
"/quack add a Flappy Bird game and open a PR"
  → quack clones the repo, writes the code, commits, and opens a PR
  → the final answer is posted back as a comment
```

## Two ways to drive it

**Labels are persistent capability FLAGS, not one-shot triggers.** A label's initial action fires once when applied; the capability it names stays live for as long as the label is present, and removing it turns that capability off. Applying one needs repo write access, so the label itself doubles as the permission model:

| Label (default name) | What it does |
|---|---|
| `quack:plan` | Plans once when applied. While present, revises the plan on an explicit `/quack …` address or quote-reply. |
| `quack:implement` | Implements the approved plan once, commits locally, and opens a PR pre-labeled for review. Add `quack:partial-fix` first if the PR shouldn't auto-close the issue. |
| `quack:review` (configurable) | Reviews the PR once. Also fires automatically on PR open if the `pr_opened` trigger is enabled. While the label is present, a PR comment consisting of `/review` (from a write/admin author) re-runs the review - GitHub coalesces a fast label remove+add into no webhook, so cycling the label can't. |
| `quack:fix` | Keeps the PR green: fixes it on **any** CI/CD failure while it carries this label, not only when freshly applied - re-applying it re-arms after a stop and, if CI is already failing, fixes it immediately. One fix attempt per failure (see "CI auto-heal" below). |
| `quack:merge` | Squash-merges the PR - but only at the intersection of a human applying this label **and** quack's own latest review having approved it. Anything else gets an explanatory comment instead of a merge. |

Every label handler reacts with 👀 the instant it fires, before the run even starts.

**`/quack <request>`, at the START of a line,** is the conversational path - free-form, for anything that doesn't fit a label: "review this PR", "what did you mean by that finding?", "fix the typo in the README". The token must open a line (leading whitespace is fine); it does not match inside a sentence, and it does not match a quoted `> /quack …` reply, so replying to an earlier mention never re-fires it. A mention on a PR that isn't asking for work (a question, a clarification) is answered directly from the conversation so far; it never re-triggers a review. A mention that does ask for review or code changes runs the same way a label would.

**Authorship is the flag on PRs quack opened itself.** No label is needed: quack replies on its own PRs, and a `request_changes` review on one it authored engages it to address the findings - the same "keep it green" treatment `quack:fix` gives a labeled PR, just triggered by having written the PR rather than by a label.

Every path shares one session per issue/PR thread, so context (a plan, a prior review) carries forward regardless of which trigger drove which step. Only one run is ever in flight per thread - a trigger that arrives mid-run is deduplicated with a 👀 rather than started concurrently.

### CI auto-heal (`quack:fix`)

While a PR carries `quack:fix` - or quack itself authored the PR - any CI/CD failure dispatches a fix run on the PR's existing session: it diagnoses the failing checks, makes the smallest fix, and the trust gate re-pushes the PR in place. **One fix attempt per failure**: if quack's own fix push also fails CI, it stops and comments why instead of trying again - the guard checks whether the commit that just failed CI is one quack itself made (every quack commit carries a fixed system identity), not a counter, so a later failure caused by a genuinely new (human) commit heals again with no relabeling and nothing to reset. Re-applying `quack:fix` clears a prior stop and, if CI is currently failing, fixes it right away.

### What a triggered run sees

Every dispatch builds a structured envelope, not a hand-assembled paragraph: `<permissions>` (this run's grant, below), `<deliverable>` (the one thing this run should produce), the hoisted `<issue>`/`<pull_request>` title and description, `<comments>` (every comment on first load, only what's new, edited, or deleted since the last dispatch on resume), `<changed_files>` on a PR (name/additions/deletions - no patches; agents read those off the clone), and the triggering `<event>` - GitHub's own webhook JSON, filtered by a fixed drop-list (`node_id`, every `*_url`, `avatar_url`, `reactions`) and nothing else: fields are dropped, never renamed or reshaped, so the model sees the same GitHub shape it's seen a million times in training.

A `<context dir>` in the envelope points at a directory, sibling to the working clone, of the untruncated GitHub API responses the envelope itself only summarizes or caps: `issue.json`, `comments.json`, `pull.json`, `files.json`, `commits.json`, `reviews.json`, `review-comments.json`, `check-runs.json` (plus `annotations-*.json` for any failing check, on a CI-triggered run), `linked-issue-*.json`, `timeline.json`. Sandboxed agents get it mounted read-only.

The orchestrator gets the full envelope above. A plan's individual nodes get a narrower ask-only slice instead - permissions, deliverable, the hoisted title/description, comments, and (on a CI-fix run) that node's own failing-check detail - never the full file list or the raw event, so a node's own task isn't crowded out by planning-scale evidence it has no use for.

### Permissions (the grant)

Computed once per dispatch, from the labels currently on the issue/PR, whether quack itself authored the PR, and a fork check (quack can't push to a fork head): `join_issue_conversation`, `open_pr`, `post_review`, `join_pr_conversation`, `push_commits_to_pr`. It rides along in the envelope's `<permissions>` block as information for the model, but the model stating a permission doesn't grant it - the trust gate's delivery step is the actual enforcement point. A staged PR, review, or comment the grant doesn't cover is refused, logged, and reported as a failed delivery; it never ships and never gets silently dropped without a trace.

### How review actually works

The code-reviewer is an **external ACP agent** (opencode, spawned per round) - it has no quack tools of its own, so it can't build up a review with API calls. Instead it reads the diff with its own tools and ends its answer with a structured tail:

```text
VERDICT: approve | request_changes | comment
FINDINGS:
- path/to/file.go:42: the finding text
- other/file.ts:7: another finding
```

Quack's trust gate parses that tail (`internal/vetting/answerreview.go`) into a native GitHub review - inline comments anchored to file and line, plus a summary and verdict - and posts it exactly once, after the gate passes. If the gate doesn't pass, nothing is posted from that round. A reviewer answer with no verdict at all still posts as a plain comment-review rather than looping forever waiting for a tail that will never come.

One wrinkle: GitHub won't let quack formally approve or request changes on a PR it authored itself (self-review, 422). When that happens, quack's review posts as a `COMMENT`-event review instead - still with real inline comments - and carries the actual verdict in a hidden marker in the review body. `quack:merge` reads that marker (falling back to a formal review state when quack didn't author the PR) to decide whether it's allowed to merge.

## Setup

### 1. Register a GitHub App

GitHub → **Settings → Developer settings → GitHub Apps → New GitHub App**.

- **Name**: e.g. `quack-<yourorg>`.
- **Homepage URL**: anything (your repo).
- **Webhook URL**: `https://<your-host>/api/v1/github/webhook`.
- **Webhook secret**: generate a strong random string; you'll put it in `QUACK_GITHUB_WEBHOOK_SECRET`.

### 2. Permissions (least privilege)

Under **Permissions → Repository**:

| Permission | Access | Why |
|---|---|---|
| Contents | Read & write | clone + push branches |
| Issues | Read & write | read comments, post replies, react |
| Pull requests | Read & write | open PRs, post reviews, merge |
| Metadata | Read (mandatory) | required by GitHub |

Grant nothing else.

### 3. Subscribe to events

Under **Subscribe to events**, check: **Issue comment**, **Issues**, **Pull request**, **Pull request review** (for `quack:fix`'s CI auto-heal, also check **Workflow run**). Issue comment alone is enough if you only want the `/quack` mention path - the label workflow needs the rest, and the authorship-based engagement on quack's own PRs needs Pull request review.

### 4. Generate the private key

App page → **Private keys → Generate a private key**. A `.pem` downloads. Keep it secret.

### 5. Install the App

App page → **Install App** → pick the account/org → choose the target repos. Note the **Client ID** (`Iv23li…`) on the App page → *About* - that's the recommended JWT issuer (the numeric App ID also works).

### 6. Configure quack

Add an `extensions.github` block to `quack.yaml`. Secrets are `${ENV}` references - a literal is a startup error:

```yaml
extensions:
  github:
    client_id: Iv23liExample     # recommended issuer, OR:
    # app_id: 123456             # legacy alternative - set exactly one
    private_key: ${QUACK_GITHUB_PRIVATE_KEY}          # PEM contents via env, OR:
    # private_key_path: /run/secrets/quack-github.pem # path to the .pem
    webhook_secret: ${QUACK_GITHUB_WEBHOOK_SECRET}
    mention: "/quack"                    # default; must open a line - see "Two ways to drive it"
    allowed_users: [yourgithublogin]      # empty denies every human-invoked trigger
    triggers: [mention, pr_opened, label, issue_plan, issue_implement, merge, ci_fix]
    # labels:                            # defaults shown; override any of them
    #   plan: "quack:plan"
    #   implement: "quack:implement"
    #   review: "quack-auto-review"
    #   merge: "quack:merge"
    #   fix: "quack:fix"

# For code tasks the agent must be allowed to push:
workspace:
  git_push: true
  guards:
    git_push: judge   # see "Non-interactive guard policy" below
```

`allowed_users` gates every human-invoked trigger (mention, labels) by GitHub login, case-insensitively - seed it or quack won't respond. The automatic `pr_opened` auto-review is exempt (nobody applied it). Bot comments are always ignored, so quack never re-triggers on its own posts.

Environment:

```bash
export QUACK_GITHUB_WEBHOOK_SECRET='the-secret-you-set-in-step-1'
export QUACK_GITHUB_PRIVATE_KEY="$(cat quack-github.pem)"
```

The App's client secret is **not** used - quack authenticates as the App by signing a JWT with the private key, not via OAuth.

The installation token doubles as quack's git credential: when a tool clones or pushes a `github.com` URL and no static credential matches, the extension mints a short-lived token for that repo's installation automatically (never written to disk, never in a URL). A static PAT (`workspace.git_credentials`) still works and wins if both are configured.

When `extensions.github` is absent, the extension isn't built at all - no tools, and `/api/v1/github/webhook` returns `404`.

### 7. Expose the endpoint

`/api/v1/github/webhook` must be reachable from GitHub over HTTPS. In production, terminate TLS at a reverse proxy in front of quack.

**Local development** - forward GitHub's webhooks to your machine:

```bash
# Option A: GitHub CLI
gh webhook forward --repo=<owner>/<repo> --events=issue_comment,issues,pull_request \
  --url=http://localhost:8080/api/v1/github/webhook

# Option B: smee.io - create a channel at https://smee.io, set it as the App's
# Webhook URL, then:
npx smee-client --url https://smee.io/<channel> \
  --target http://localhost:8080/api/v1/github/webhook
```

### 8. Verify it works

1. Start quack with the config above; the log shows `github extension enabled`.
2. On an installed repo, open an issue and comment `/quack say hello` (the token must open the line).
3. Watch the logs: `github webhook received` → `github run dispatched` → `github comment posted`.
4. The App replies on the issue with the run's answer.

A `401` in GitHub's *Recent Deliveries* (App → *Advanced*) means the `webhook_secret` doesn't match.

## Non-interactive guard policy

A webhook-driven run has no human at a terminal, so it can never clear a `confirm`-tier guard - it pauses (`node_needs_input`) and ends without performing that operation, rather than hanging forever. Quack's shipped default puts `git_push` on `judge+confirm`. For the App to push branches and open PRs autonomously, drop it to `judge`:

```yaml
workspace:
  git_push: true
  guards:
    git_push: judge   # was judge+confirm - the human tier can't run in a webhook
```

With `judge`, the independent judge model is the only safety check on a push. Surfacing a paused confirmation as a GitHub comment and resuming when a maintainer replies is a possible future improvement, not built today.

## Security

- **Signatures are always verified.** `X-Hub-Signature-256` is checked with a constant-time compare against the raw body before anything else happens. This is the trust boundary; there's no bypass.
- **Least privilege.** Grant only the permissions listed above.
- **Private-key handling.** Provide it via a file path (ideally a mounted secret) or an env var - never a literal in `quack.yaml` (enforced at load). It's never logged.
- **Short-lived tokens.** Installation tokens last about an hour; quack caches and refreshes them, and they travel only as per-child-process env vars.
- **Bot comments are ignored.** Quack never reacts to another bot's comments, including its own - so it can't be made to re-trigger itself.
