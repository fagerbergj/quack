# GitHub App extension

Quack can run as a **GitHub App**: it receives GitHub webhook events,
authenticates as the App installation, dispatches the orchestrator on a repo,
and replies on the issue/PR. This is quack's first **Extension** — a bundled
unit that owns one auth context and contributes in both directions:

- **Outbound** — tools the agent calls, authed as the App installation token:
  `github_comment` (post a comment), `github_pull_request` (open a PR), the
  **review-draft** tools (`github_add_review_comment` / `github_list_review_comments`
  / `github_delete_review_comment` / `github_submit_review`) that build up one
  native PR review with inline comments, and the **discussion** tools
  (`github_list_pr_comments` / `github_reply_to_review_comment` /
  `github_react_to_comment`) that read and react to a PR's existing threads (see
  [Review tools](#review-tools)). The
  existing git tools (`git_clone` / `git_push` / …) also authenticate through
  this extension's installation token instead of a static PAT while it is
  active.
- **Inbound** — a signature-verified webhook route (`/api/v1/github/webhook`)
  that dispatches runs on triggering events.

The whole loop:

```
issue_comment "@quack add a Flappy Bird game and open a PR"   (inbound webhook)
  → verify X-Hub-Signature-256 (HMAC-SHA256, constant-time)
  → mint installation token for installation.id
  → Orchestrator.Run(task) in a session keyed to repo/issue
  → agent clones repo (git auth = installation token), writes code,
    commits, pushes a branch, opens a PR (github_pull_request)   (outbound)
  → the final answer is posted back as a comment                 (outbound)
```

---

## Review tools

The code-reviewer submits its findings as **one native GitHub PR review** —
inline comments anchored to file+line, plus a summary and a verdict — instead of
a scatter of separate conversation comments. It builds that review up with four
CRUD tools backed by a process-local, per-PR draft:

| Tool | Args | Effect |
|------|------|--------|
| `github_add_review_comment` | `owner`, `repo`, `pull_number`, `path`, `line`, `body`, optional `side` (`LEFT`/`RIGHT`, default `RIGHT`), optional `start_line`+`start_side` (multi-line range) | Validates the location against the PR diff, then appends the comment to the draft. Returns its index + draft size. |
| `github_list_review_comments` | `owner`, `repo`, `pull_number` | Returns the pending draft comments, each with its index. |
| `github_delete_review_comment` | `owner`, `repo`, `pull_number`, `index` | Removes one draft comment (edit = delete + re-add). Remaining indices shift down. |
| `github_submit_review` | `owner`, `repo`, `pull_number`, `body`, `event` (`COMMENT`/`REQUEST_CHANGES`/`APPROVE`) | Posts the whole draft as one review (`POST /pulls/{n}/reviews`) and clears it. |

**Deterministic inline-location validation.** `github_add_review_comment` fetches
the PR diff (`GET /repos/{owner}/{repo}/pulls/{n}/files`, cached briefly per PR)
and checks that `path` is a changed file and `line` is a commentable line in that
file's diff hunks (added/context lines on the `RIGHT` side, removed/context on the
`LEFT`). If not, it rejects the comment with a clear error naming the problem and
the commentable line range — so a bad line ref is caught **at add time**, when the
agent can fix it, rather than 422-ing the whole review at submit. Because every
drafted comment was already location-validated, `github_submit_review` can't 422
on a bad line.

**Why a draft store, not one batch call.** The draft is the reviewer's
*externalized memory*. A single "collect every finding then submit once" call
fails when the agent's context is compacted mid-review (older findings summarized
or dropped) — by submit time it may have forgotten half of them. Recording each
finding the moment it's spotted persists it outside the context window. The store
is process-local and ephemeral (a review is drafted and submitted within one agent
run in one process); the upgrade path, if drafts ever need to outlive a run, is
GitHub's native *pending review* (create-review-without-event → add comments →
submit later) — not built today.

### Reading & reacting to existing discussion

So the reviewer sees prior context before adding its own (and doesn't repeat what
was already said), three more tools read and react to a PR's existing threads:

| Tool | Args | Effect |
|------|------|--------|
| `github_list_pr_comments` | `owner`, `repo`, `pull_number` | Returns the PR's existing inline review comments (path/line/body/user/in_reply_to_id), conversation comments, and submitted reviews (body/state/user). |
| `github_reply_to_review_comment` | `owner`, `repo`, `pull_number`, `comment_id`, `body` | Posts an in-thread reply to an existing inline review comment (`POST /pulls/{n}/comments/{id}/replies`). |
| `github_react_to_comment` | `owner`, `repo`, `comment_id`, `comment_type` (`review_comment`/`issue_comment`), `content` (`+1`/`-1`/`laugh`/`hooray`/`confused`/`heart`/`rocket`/`eyes`) | Adds an emoji reaction — a low-cost way to close a loop (acknowledge/agree/"seen") without writing a comment. |

**Deterministic 👀 on mention.** Independent of the model, the webhook handler
reacts with 👀 (eyes) to the mentioning comment the instant a valid `@quack`
mention arrives — a code-level "quack saw it, it's on it" that doesn't wait on the
run. It reuses the reaction HTTP path (`reactToComment`, shared with
`github_react_to_comment`) and is best-effort: a failed reaction is logged at WARN
and never blocks the run dispatch.

**Follow-up (not built): resolving a review thread.** Marking a review thread
*resolved* is GitHub **GraphQL**-only (`resolveReviewThread` mutation) — there is
no REST equivalent. The tools here are all REST; thread resolution is a documented
follow-up, skipped for now.

---

## Design (spec)

### Scope (in)

- **App auth**: App JWT (RS256) → per-installation access token via
  `POST /app/installations/{id}/access_tokens`, cached per installation until
  shortly before expiry. Installation resolved from a repo's `owner/repo` for
  the tools/git seam (`GET /repos/{owner}/{repo}/installation`).
- **Webhook receiver**: `POST /api/v1/github/webhook` — reads the raw body,
  verifies `X-Hub-Signature-256`, dispatches by `X-GitHub-Event`, returns
  **200 fast** and runs the orchestrator **async** (GitHub's ~10s timeout).
- **One trigger**: `issue_comment` `created` whose body contains the mention
  (default `@quack`). Task = the text after the mention. Applies to both
  issues and PRs (GitHub sends PR comments as `issue_comment`).
- **Outbound tools**: `github_comment`, `github_pull_request`, the review-draft
  CRUD (`github_add_review_comment`, `github_list_review_comments`,
  `github_delete_review_comment`, `github_submit_review`), and the discussion
  tools (`github_list_pr_comments`, `github_reply_to_review_comment`,
  `github_react_to_comment`).
- **Git-credential integration**: the App is a dynamic git credential source
  for `github.com` (installation token, cached), resolved from the clone/remote
  URL. The static-PAT path (`workspace.git_credentials`) still works and wins
  when both match.
- **Extension seam**: `internal/extension.Extension` (Name / Tools /
  RegisterRoutes) + the GitHub impl in `internal/github`. Off by default.

### Scope (out — follow-ups)

- Full event matrix (`push`, `pull_request` opened/labeled, `issues`).
- Comment-driven confirm-resume (surface a confirmation AS a comment, resume
  when a maintainer replies).
- Multi-installation management UI, rate-limit/retry hardening, check-run status
  reporting, richer tools (create issue, request review, labels).
- A plugin loader / marketplace / dynamic discovery. One interface, one impl,
  startup wiring — nothing more.

### Forbidden actions

- **Never** skip signature verification, and always compare in constant time.
  Missing/invalid signature ⇒ `401`.
- **Never** log the private key, the webhook secret, or any installation/JWT
  token.
- **Never** run the whole agent synchronously in the webhook handler.
- Secrets (`private_key`, `webhook_secret`) **must** be `${ENV}` references in
  `quack.yaml` — a literal is a startup error (same rule as
  `workspace.git_credentials`).

### Interfaces it depends on

- `orchestrator.Orchestrator.Run` (dispatch) + `LatestAnswer` (read the final
  reply after the run drains).
- `internal/tools.GitTokenSource` (the git-credential seam) + `functiontool`
  for the outbound tools.
- `chi.Router` (route mount, mirrors the MCP mount in `router.go`).
- `crypto/hmac`, `crypto/sha256`, `crypto/rsa`, `golang-jwt/jwt/v5`,
  `net/http` — no `go-github` / `ghinstallation` (see "Auth" below).

### Output contract (webhook → run → reply)

| Situation | HTTP | Side effect |
|-----------|------|-------------|
| Valid sig, `issue_comment` created, body has the mention | `202` | async run; final answer posted as a comment; code tasks push a branch + open a PR |
| Valid sig, comment **without** the mention | `200` | no-op |
| Valid sig, unhandled event type | `200` | no-op |
| Missing / invalid signature | `401` | none |
| Extension not configured | `404` | route not mounted |

### Test cases

1. `issue_comment.created` with `@quack fix the typo` and a valid signature →
   handler returns fast; the orchestrator is invoked with a message that
   carries the task text + `owner/repo`/issue number + clone URL; the poster is
   called with the run's answer. (Orchestrator + comment poster stubbed.)
2. Same event, body `just chatting, no mention` → `200`, orchestrator **not**
   invoked.
3. `X-GitHub-Event: star` (unhandled) → `200`, no-op.
4. Tampered body (signature computed over different bytes) → `401`, no-op.
5. App auth: JWT carries `iss`=the configured issuer (client id or app id) and a ≤10-min expiry; the installation
   token exchange hits the stubbed endpoint once, and a second call within
   expiry is served from cache (no re-fetch).

---

## Auth: why stdlib + `golang-jwt/jwt`, not `go-github`/`ghinstallation`

The App flow is *one* signed JWT plus a handful of REST calls (mint token, post
comment, open PR, resolve installation). `golang-jwt/jwt/v5` is already in the
module graph; the REST calls are four small `net/http` requests. Pulling in
`go-github` (a large generated client) and `ghinstallation` to save ~80 lines
is a poor trade for a self-hosted binary. So: `jwt/v5` for the RS256 JWT,
stdlib `net/http` for the REST, stdlib `crypto/hmac` for the webhook signature.

---

## Non-interactive guard policy (important)

Quack's guard ladder has a `confirm` tier that pauses a node for a human
approve/deny (`workspace.guards`). A webhook-driven run has **no human at a
terminal**. By default nothing is on the `confirm` tier, so a webhook run
executes autonomously, with the `judge` tier (an independent model allow/deny)
providing automated safety for risky ops.

**If** a tool is on the `confirm` tier, a webhook run that reaches it pauses
(`node_needs_input`) and the run ends **without performing that operation** —
fail-safe (the risky op does not execute; nothing hangs forever), but the task
only half-completes because no one can answer.

**This matters for `git_push`:** quack's *shipped default* is
`git_push: judge+confirm` (see `docs/configuration.md`, `workspace.guards`). A
webhook run that tries to push will therefore pause at the confirm tier and the
push will not happen. **For the App to open PRs autonomously, override it to the
`judge` tier:**

```yaml
workspace:
  git_push: true
  guards:
    git_push: judge   # was judge+confirm; the human tier can't run in a webhook
```

The richer future option (surface the confirmation as a GitHub comment and
resume when a maintainer replies) is an explicit follow-up, not built here.

---

## Setup

### 1. Register a GitHub App

GitHub → **Settings → Developer settings → GitHub Apps → New GitHub App**.

- **Name**: e.g. `quack-<yourorg>`.
- **Homepage URL**: anything (your repo).
- **Webhook URL**: `https://<your-host>/api/v1/github/webhook`.
- **Webhook secret**: generate a strong random string; you will also put it in
  `QUACK_GITHUB_WEBHOOK_SECRET`.

### 2. Permissions (least privilege)

Under **Permissions → Repository**:

| Permission | Access | Why |
|------------|--------|-----|
| Contents | Read & write | clone + push branches |
| Issues | Read & write | read the comment, post replies |
| Pull requests | Read & write | open PRs, comment on PRs |
| Metadata | Read (mandatory) | required by GitHub |

Grant nothing else.

### 3. Subscribe to events

Under **Subscribe to events**: check **Issue comment**. (Add **Pull request** /
**Issues** later if you extend the trigger set — not needed for the MVP.)

### 4. Generate the private key

On the App's page → **Private keys → Generate a private key**. A `.pem`
downloads. Keep it secret.

### 5. Install the App

App page → **Install App** → pick the account/org → choose the target repos.
Note the **Client ID** (`Iv23li…`) and **App ID** (both on the App page →
*About*). Either identifies the App as the JWT issuer; GitHub now recommends the
Client ID.

### 6. Configure quack

Add an `extensions.github` section to `quack.yaml`. Secrets are `${ENV}`
references (a literal is a startup error). Provide **exactly one** issuer —
`client_id` (recommended) or `app_id`:

```yaml
extensions:
  github:
    client_id: Iv23liExample     # recommended issuer (App page → About), OR:
    # app_id: 123456             # legacy alternative — set exactly one, not both
    # ONE of these two:
    private_key_path: /run/secrets/quack-github.pem   # path to the .pem, OR
    private_key: ${QUACK_GITHUB_PRIVATE_KEY}          # PEM contents via env
    webhook_secret: ${QUACK_GITHUB_WEBHOOK_SECRET}
    mention: "@quack"          # default; the trigger phrase

# For code tasks the agent must be allowed to push:
workspace:
  git_push: true
  # No git_credentials entry for github.com is needed — the extension supplies
  # the installation token. A static PAT still works and wins if present.
```

Environment:

```bash
export QUACK_GITHUB_WEBHOOK_SECRET='the-secret-you-set-in-step-1'
# if using private_key (not private_key_path):
export QUACK_GITHUB_PRIVATE_KEY="$(cat quack-github.pem)"
```

The App's **client secret is NOT used** — quack authenticates as the App by
signing a JWT with the private key (App auth), not via the OAuth
client-id/client-secret flow. Only `client_id` (or `app_id`) and the private key
are configured; `client_id` and `app_id` are identifiers, not secrets, so they
may be literals.

The installation token becomes the git credential automatically: when a tool
clones/pushes a `github.com` URL and no static credential matches, the
extension resolves the repo's installation and mints a short-lived token,
injected via quack's askpass mechanism (never written to disk, never in a URL).

When `extensions.github` is absent, the extension is not built: no tools, and
`/api/v1/github/webhook` returns `404`.

### 7. Expose the endpoint

Your `/api/v1/github/webhook` must be reachable from GitHub over HTTPS. In
production, terminate TLS at a reverse proxy in front of quack.

**Local development** — forward GitHub's webhooks to your machine:

```bash
# Option A: GitHub CLI
gh webhook forward --repo=<owner>/<repo> --events=issue_comment \
  --url=http://localhost:8080/api/v1/github/webhook

# Option B: smee.io — create a channel at https://smee.io, set it as the App's
# Webhook URL, then:
npx smee-client --url https://smee.io/<channel> \
  --target http://localhost:8080/api/v1/github/webhook
```

### 8. Verify it works

1. Start quack with the config above; the log shows
   `github extension enabled`.
2. On an installed repo, open an issue and comment `@quack say hello`.
3. Watch the logs: `github webhook received` → `github run dispatched` →
   `github comment posted`.
4. The App replies on the issue with the run's answer.

Signature-verification failures log `github webhook: signature verification
failed` and return `401` — if GitHub's *Recent Deliveries* (App → *Advanced*)
shows a 401, the `webhook_secret` doesn't match.

---

## Security

- **Always verify signatures.** `X-Hub-Signature-256` is checked with
  `hmac.Equal` (constant time) against the raw body before any dispatch. This
  is the trust boundary; there is no bypass.
- **Least privilege.** Grant only the four permissions above.
- **Private-key handling.** Provide it via a file path (`private_key_path`,
  ideally a mounted secret) or an env var (`private_key: ${...}`) — never a
  literal in `quack.yaml` (enforced at load). It is never logged.
- **Short-lived tokens.** Installation tokens last ~1h; quack caches them and
  refreshes shortly before expiry. They travel only as per-child-process env
  vars through the askpass seam.
- **Non-interactive guard policy.** See the section above: webhook runs rely on
  the `judge` tier, not the human `confirm` tier.
