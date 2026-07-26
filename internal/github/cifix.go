// PR self-heal (#254): a failing CI run on a PR that opted in via the monitor
// label dispatches an implement-style fix run on the PR's EXISTING session -
// quack continues with the plan, prior work and diff context it already has,
// fixes on the PR's head branch in the still-provisioned clone, and the trust
// gate's delivery spine re-pushes the PR in place. The loop is bounded and the
// bound is DURABLE (store.GithubFixState): quack's own fix push re-runs CI, so
// the next failure webhook IS the retry - the persisted counter is what stops
// it thrashing, across process restarts.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/store"
)

// fixContextTimeout bounds the pre-dispatch API phase of a fix trigger
// (labels, check runs, annotations) - the run itself gets runTimeout.
const fixContextTimeout = 2 * time.Minute

// workflowRunPayload is the subset of GitHub's workflow_run webhook we use.
// PullRequests is empty for fork-head PRs - quack cannot push to a fork's
// branch anyway, so those are simply never auto-healed.
type workflowRunPayload struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		Name         string `json:"name"`
		HeadSHA      string `json:"head_sha"`
		Conclusion   string `json:"conclusion"`
		HTMLURL      string `json:"html_url"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"workflow_run"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		CloneURL      string `json:"clone_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// handleWorkflowRun is the CI auto-heal trigger: a workflow that completed
// with a failure on a PR carrying the monitor label dispatches a bounded fix
// run. Deliberately NOT bot-sender-gated: quack's own fix push re-triggers CI
// and that failure webhook is the retry - the durable attempt counter, not the
// sender, is the loop bound.
func (e *Extension) handleWorkflowRun(w http.ResponseWriter, body []byte) {
	var p workflowRunPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Action != "completed" || !e.triggers["ci_fix"] {
		w.WriteHeader(http.StatusOK)
		return
	}
	switch p.WorkflowRun.Conclusion {
	case "failure", "timed_out":
	default:
		w.WriteHeader(http.StatusOK) // success/cancelled/skipped: nothing to heal
		return
	}
	// No store means the attempt counter cannot survive a restart - refuse to
	// run an unboundable loop rather than degrade to in-memory counting.
	if e.store == nil {
		slog.Warn("github: ci_fix trigger needs a store for its durable retry bound; ignoring workflow_run",
			"component", "github", "repo", p.Repository.Owner.Login+"/"+p.Repository.Name)
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(p.WorkflowRun.PullRequests) == 0 {
		w.WriteHeader(http.StatusOK) // branch push with no open same-repo PR, or a fork PR
		return
	}
	slog.Info("github webhook received", "component", "github",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "workflow", p.WorkflowRun.Name,
		"conclusion", p.WorkflowRun.Conclusion, "head_sha", p.WorkflowRun.HeadSHA,
		"installation", p.Installation.ID)
	for _, pr := range p.WorkflowRun.PullRequests {
		go e.autoHeal(p, pr.Number)
	}
	w.WriteHeader(http.StatusAccepted)
}

// autoHeal gates one PR's auto-heal and dispatches the fix run. Order is
// cheapest-and-safest first: label gate, per-commit dedup, attempt bound - and
// the attempt is persisted BEFORE the run so a crash mid-run never refunds it.
// Every store failure fails CLOSED (skip the run): an unbounded loop is worse
// than a missed heal.
func (e *Extension) autoHeal(p workflowRunPayload, number int) {
	owner, repo := p.Repository.Owner.Login, p.Repository.Name
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	sha := p.WorkflowRun.HeadSHA

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	_, _, _, labels, err := e.app.issueMeta(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: auto-heal label check failed; skipping", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	if !hasLabel(labels, e.labels.Monitor) {
		return // no monitor label ⇒ no auto-heal, ever
	}

	st, err := e.store.GetGithubFixState(ctx, sessionID)
	if err != nil {
		slog.Warn("github: auto-heal state read failed; skipping", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	var attempts int
	var exhausted bool
	if st != nil {
		if st.LastSHA == sha {
			// Another failing workflow on a commit already handled (CI usually
			// runs several) - one heal per head commit.
			slog.Info("github: auto-heal already handled this head commit; skipping",
				"component", "github", "repo", owner+"/"+repo, "pr", number, "sha", sha)
			return
		}
		attempts, exhausted = st.Attempts, st.Exhausted
	}

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, owner, repo, number, text); err != nil {
			slog.Error("github: auto-heal comment failed", "component", "github",
				"repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}

	if attempts >= e.fixAttempts {
		// Remember this sha either way so sibling workflow failures (and any
		// post-exhaustion failure on the same commit) stay silent.
		save := store.GithubFixState{ChatID: sessionID, Attempts: attempts, LastSHA: sha, Exhausted: true}
		if err := e.store.SetGithubFixState(ctx, save); err != nil {
			slog.Warn("github: auto-heal exhausted-state write failed", "component", "github",
				"repo", owner+"/"+repo, "pr", number, "err", err)
		}
		if !exhausted {
			checksText := e.failingChecksText(ctx, owner, repo, sha, p.WorkflowRun.Name, p.WorkflowRun.HTMLURL)
			comment(fmt.Sprintf("⚠️ Auto-heal stopped: %d fix attempt(s) on this PR did not get CI green. Still failing:\n\n%s\n\nI won't retry on my own - re-apply the `%s` label to reset the counter and retry, or mention me with specific guidance.",
				attempts, checksText, e.labels.Monitor))
		}
		return
	}

	attempts++
	if err := e.store.SetGithubFixState(ctx, store.GithubFixState{ChatID: sessionID, Attempts: attempts, LastSHA: sha}); err != nil {
		slog.Error("github: auto-heal attempt persist failed; refusing to run without a durable bound",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}

	checksText := e.failingChecksText(ctx, owner, repo, sha, p.WorkflowRun.Name, p.WorkflowRun.HTMLURL)
	comment(fmt.Sprintf("🔧 CI failed on `%s` - attempting an automatic fix (attempt %d of %d).", shortSHA(sha), attempts, e.fixAttempts))

	// Continue the PR's existing session under the identity it was written
	// with - session reuse is the point of this feature (#254).
	login := e.store.SessionUserForChat(ctx, sessionID)
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = login
	synthetic.Repository.Name = repo
	synthetic.Repository.Owner.Login = owner
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	// isLabelTrigger stays false: a fix run must NOT reset the session.

	slog.Info("github: auto-heal dispatching fix run", "component", "github",
		"repo", owner+"/"+repo, "pr", number, "attempt", attempts, "sha", sha)
	e.dispatch(synthetic, fixTask(fmt.Sprintf("CI is failing on this pull request (auto-heal attempt %d of %d).", attempts, e.fixAttempts), checksText))
}

// runFixLabel is the explicit-request trigger: a human applied the fix label,
// so fix whatever checks are failing RIGHT NOW, once. No attempt counting -
// each label application is one authorized run (it only fires on the "labeled"
// action, so it cannot loop).
func (e *Extension) runFixLabel(p pullRequestPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	checks, err := e.failingChecks(ctx, owner, repo, p.PullRequest.Head.SHA)
	if err != nil {
		slog.Warn("github: fix-label check fetch failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		if perr := e.app.postIssueComment(ctx, owner, repo, number,
			fmt.Sprintf("Couldn't read this PR's checks (%v) - not running blind. Re-apply the `%s` label to retry.", err, e.labels.Fix)); perr != nil {
			slog.Error("github: fix-label comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", perr)
		}
		return
	}
	if len(checks) == 0 {
		if perr := e.app.postIssueComment(ctx, owner, repo, number,
			"Nothing is failing on this PR right now - mention me with what you want fixed instead."); perr != nil {
			slog.Error("github: fix-label comment failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", perr)
		}
		return
	}

	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.Title = p.PullRequest.Title
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = p.Sender.Login
	synthetic.Repository.Name = repo
	synthetic.Repository.Owner.Login = owner
	synthetic.Repository.CloneURL = p.Repository.CloneURL
	synthetic.Repository.DefaultBranch = p.Repository.DefaultBranch
	synthetic.Installation.ID = p.Installation.ID
	// isLabelTrigger stays false: a fix continues the PR's session, it never resets it.

	e.dispatch(synthetic, fixTask(fmt.Sprintf("@%s asked me (via the `%s` label) to fix the failing checks on this pull request.", p.Sender.Login, e.labels.Fix), renderFailingChecks(checks)))
}

// resetFixState re-arms auto-heal when a human (re-)applies the monitor label
// - the documented "re-apply the label to retry" convention. Deterministic, no
// model run; best-effort 👀 acknowledges it happened.
func (e *Extension) resetFixState(p pullRequestPayload) {
	owner, repo, number := p.Repository.Owner.Login, p.Repository.Name, p.Number
	ctx, cancel := context.WithTimeout(context.Background(), reactionTimeout)
	defer cancel()
	sessionID := fmt.Sprintf("github-%s-%s-%d", owner, repo, number)
	if err := e.store.DeleteGithubFixState(ctx, sessionID); err != nil {
		slog.Warn("github: fix-state reset failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	slog.Info("github: auto-heal re-armed by monitor label", "component", "github",
		"repo", owner+"/"+repo, "pr", number, "user", p.Sender.Login)
	if _, err := e.app.reactToIssue(ctx, owner, repo, number, "eyes"); err != nil {
		slog.Warn("github: monitor-label ack reaction failed", "component", "github",
			"repo", owner+"/"+repo, "pr", number, "err", err)
	}
}

// hasLabel reports whether names includes label.
func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

// fixTask frames a fix run's task. The wording is implement-and-deliver on
// purpose (fix + commit/branch) so vetting.ImplementationIntent routes it as an
// implement run, and dispatch's runMessage appends the stage_pr delivery
// guidance - the fix rides the existing spine, push and PR update included.
func fixTask(intro, checksText string) string {
	var b strings.Builder
	b.WriteString(intro)
	b.WriteString(" Diagnose the failures below and fix them IN PLACE: make the smallest change that gets the checks green, run the repo's own checks locally to verify, and commit your work on the PR's existing head branch (already checked out for you). Do not start a new branch and do not open a new pull request - your commit updates THIS pull request.\n\nFailing checks:\n")
	b.WriteString(checksText)
	return b.String()
}

// Caps bounding the failure context injected into a fix task - annotations and
// summaries come from CI and can be arbitrarily large.
const (
	maxFailingChecks       = 5
	maxAnnotationsPerCheck = 10
	maxChecksContextRunes  = 6000
)

// failingCheck is one failed check run, rendered down to what a fix prompt
// needs.
type failingCheck struct {
	Name        string
	Summary     string
	URL         string
	Annotations []string
}

// failingChecks fetches the commit's check runs and keeps the failed ones,
// with their annotations - the checks-API alternative to downloading full log
// archives (annotations carry the actual error lines for Actions jobs).
func (e *Extension) failingChecks(ctx context.Context, owner, repo, sha string) ([]failingCheck, error) {
	runs, err := e.app.listCheckRuns(ctx, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	var out []failingCheck
	for _, r := range runs {
		if r.Conclusion != "failure" && r.Conclusion != "timed_out" {
			continue
		}
		fc := failingCheck{Name: r.Name, Summary: strings.TrimSpace(r.Output.Summary), URL: r.HTMLURL}
		if fc.Summary == "" {
			fc.Summary = strings.TrimSpace(r.Output.Title)
		}
		anns, aerr := e.app.listCheckAnnotations(ctx, owner, repo, r.ID)
		if aerr != nil {
			slog.Warn("github: check annotations fetch failed; continuing without them",
				"component", "github", "repo", owner+"/"+repo, "check", r.Name, "err", aerr)
		}
		for i, a := range anns {
			if i == maxAnnotationsPerCheck {
				fc.Annotations = append(fc.Annotations, fmt.Sprintf("… and %d more", len(anns)-maxAnnotationsPerCheck))
				break
			}
			fc.Annotations = append(fc.Annotations, fmt.Sprintf("%s:%d [%s] %s", a.Path, a.StartLine, a.Level, truncate(a.Message, 300)))
		}
		out = append(out, fc)
	}
	return out, nil
}

// renderFailingChecks renders the failing checks as prompt context, bounded.
func renderFailingChecks(checks []failingCheck) string {
	var b strings.Builder
	for i, c := range checks {
		if i == maxFailingChecks {
			fmt.Fprintf(&b, "… and %d more failing checks\n", len(checks)-maxFailingChecks)
			break
		}
		fmt.Fprintf(&b, "- %s (%s)\n", c.Name, c.URL)
		if c.Summary != "" {
			fmt.Fprintf(&b, "  %s\n", truncate(c.Summary, 600))
		}
		for _, a := range c.Annotations {
			fmt.Fprintf(&b, "  %s\n", a)
		}
	}
	return truncate(b.String(), maxChecksContextRunes)
}

// failingChecksText is failingChecks+render with a graceful fallback: if the
// checks API is unreadable or reports nothing failed yet (checks can lag the
// workflow_run event), the workflow's own name and URL still ground the task.
func (e *Extension) failingChecksText(ctx context.Context, owner, repo, sha, workflowName, workflowURL string) string {
	checks, err := e.failingChecks(ctx, owner, repo, sha)
	if err != nil {
		slog.Warn("github: failing-check fetch failed; using the workflow reference only",
			"component", "github", "repo", owner+"/"+repo, "sha", sha, "err", err)
	}
	if len(checks) == 0 {
		return fmt.Sprintf("- workflow %q failed on commit %s: %s (no further check detail was readable - inspect the repo's CI config and run its checks locally to reproduce)\n", workflowName, shortSHA(sha), workflowURL)
	}
	return renderFailingChecks(checks)
}
