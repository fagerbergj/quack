// PR self-heal (#254, redesigned #656): quack:fix is a PERSISTENT capability
// flag, not a one-shot trigger. While a PR carries it (or quack itself
// authored the PR - "authorship IS the flag", #656), any CI/CD failure
// dispatches a fix run on the PR's EXISTING session: quack continues with the
// plan, prior work and diff context it already has, fixes on the PR's head
// branch in the still-provisioned clone, and the trust gate's delivery spine
// re-pushes the PR in place.
//
// The loop bound is the Forbidden section's ONE rule: ONE fix attempt per CI
// failure. quack's own fix push re-runs CI, so the next failure webhook could
// otherwise BE the retry, forever. autoHeal breaks that by checking who
// authored the commit that just failed - but only once it has already
// dispatched a fix for this PR before (a PR quack itself authored is entirely
// quack's own commits from the start, so checking authorship on the very
// FIRST failure would misread ordinary work as an already-failed fix and
// never attempt one). Once a fix has been tried, a fresh failure on quack's
// own commit can only be that fix's own CI run: it stops and waits for a
// human, rather than counting attempts against a configurable ceiling - a new
// failure caused by a HUMAN commit is always eligible again, with no label
// churn and no counter to reset.
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
	"github.com/fagerbergj/quack/internal/tools"
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

// repoInfo is the repository/installation identity common to every payload
// shape this file dispatches a fix from - factored out so beginFix takes one
// argument regardless of which webhook triggered it.
type repoInfo struct {
	Owner, Name, CloneURL, DefaultBranch string
	InstallationID                       int64
}

func (p workflowRunPayload) repoInfo() repoInfo {
	return repoInfo{p.Repository.Owner.Login, p.Repository.Name, p.Repository.CloneURL, p.Repository.DefaultBranch, p.Installation.ID}
}

func (p pullRequestPayload) repoInfo() repoInfo {
	return repoInfo{p.Repository.Owner.Login, p.Repository.Name, p.Repository.CloneURL, p.Repository.DefaultBranch, p.Installation.ID}
}

// handleWorkflowRun is the CI auto-heal trigger: a workflow that completed
// with a failure on an eligible PR (quack:fix label present, or quack itself
// authored the PR) dispatches a bounded fix run. Deliberately NOT
// bot-sender-gated: quack's own fix push re-triggers CI and that failure
// webhook is what autoHeal's one-attempt guard evaluates - the sender was
// never the loop bound.
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
	// No store means the fix state cannot survive a restart - refuse to run an
	// unboundable loop rather than degrade to in-memory tracking.
	if e.store == nil {
		slog.Warn("github: ci_fix trigger needs a store for its durable state, ignoring workflow_run",
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
// cheapest-and-safest first: eligibility (label or authorship), per-commit
// dedup, then the one-attempt guard - state is persisted BEFORE the run so a
// crash mid-run never refunds it. Every store/API failure fails CLOSED (skip
// the run): an unbounded loop is worse than a missed heal.
func (e *Extension) autoHeal(p workflowRunPayload, number int) {
	ri := p.repoInfo()
	sessionID := fmt.Sprintf("github-%s-%s-%d", ri.Owner, ri.Name, number)
	sha := p.WorkflowRun.HeadSHA

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	_, _, _, labels, _, err := e.app.issueMeta(ctx, ri.Owner, ri.Name, number)
	if err != nil {
		slog.Warn("github: auto-heal eligibility check failed; skipping", "component", "github",
			"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	eligible := hasLabel(labels, e.labels.Fix)
	if !eligible {
		// Authorship IS the flag (#656): quack fixes its own CI with no label,
		// same as it addresses its own review findings (see engageOwnPRReview).
		authored, aerr := e.authoredByQuack(ctx, ri.Owner, ri.Name, number)
		if aerr != nil {
			slog.Warn("github: auto-heal authorship check failed; skipping", "component", "github",
				"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", aerr)
			return
		}
		eligible = authored
	}
	if !eligible {
		return // no quack:fix label and not quack's own PR - never auto-heal
	}

	st, err := e.store.GetGithubFixState(ctx, sessionID)
	if err != nil {
		slog.Warn("github: auto-heal state read failed; skipping", "component", "github",
			"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	if st != nil && st.LastSHA == sha {
		// Another failing workflow on a commit already handled (CI usually
		// runs several) - one heal per head commit.
		slog.Info("github: auto-heal already handled this head commit; skipping",
			"component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "sha", sha)
		return
	}

	// The ONE-attempt guard (Forbidden: no fix→fail→fix) - but ONLY once auto-heal
	// has already attempted a fix for this PR before (st != nil): on a PR quack
	// itself authored, EVERY commit is quack's, including the very first one it
	// opened the PR with, which is not a fix attempt at all. Checking authorship
	// unconditionally would read that first, ordinary failure as "my own fix
	// already failed" and never attempt one. Once a fix HAS been dispatched
	// (st != nil), a fresh failure whose commit is quack's own can only be that
	// fix's own CI run.
	var ownCommit bool
	if st != nil {
		var cerr error
		ownCommit, cerr = e.commitAuthoredByQuack(ctx, ri.Owner, ri.Name, sha)
		if cerr != nil {
			slog.Warn("github: auto-heal could not verify the failing commit's author; skipping rather than risk a fix loop",
				"component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", cerr)
			return
		}
	}

	comment := func(text string) {
		if err := e.app.postIssueComment(ctx, ri.Owner, ri.Name, number, text); err != nil {
			slog.Error("github: auto-heal comment failed", "component", "github",
				"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		}
	}
	checksText := e.failingChecksText(ctx, ri.Owner, ri.Name, sha, p.WorkflowRun.Name, p.WorkflowRun.HTMLURL)

	if ownCommit {
		if err := e.store.SetGithubFixState(ctx, store.GithubFixState{ChatID: sessionID, LastSHA: sha, Stopped: true}); err != nil {
			slog.Warn("github: auto-heal stop-state write failed", "component", "github",
				"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		}
		comment(fmt.Sprintf("⚠️ Auto-heal stopped: my own fix on `%s` did not get CI green. Still failing:\n\n%s\nI won't attempt a second fix on my own - that's how a fix loop starts. Mention me directly with guidance, or push a change yourself.",
			shortSHA(sha), checksText))
		return
	}

	slog.Info("github: auto-heal dispatching fix run", "component", "github",
		"repo", ri.Owner+"/"+ri.Name, "pr", number, "sha", sha)
	comment(fmt.Sprintf("🔧 CI failed on `%s` - attempting an automatic fix.", shortSHA(sha)))
	e.beginFix(ctx, ri, number, sha, "CI is failing on this pull request.", checksText)
}

// fixLabelApplied handles quack:fix's "labeled" action: it re-arms auto-heal
// (clears any prior stop, so a fresh explicit human ask overrides it) and, if
// CI is CURRENTLY failing on the PR's head, fixes it right away - otherwise
// the flag just stays armed for the next failure (#655: applying it to a
// GREEN PR must do nothing, never plan a phantom review).
func (e *Extension) fixLabelApplied(p pullRequestPayload) {
	ri := p.repoInfo()
	number := p.Number
	sessionID := fmt.Sprintf("github-%s-%s-%d", ri.Owner, ri.Name, number)

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	if err := e.store.DeleteGithubFixState(ctx, sessionID); err != nil {
		slog.Warn("github: fix-state reset failed", "component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
	}
	if _, err := e.app.reactToIssue(ctx, ri.Owner, ri.Name, number, "eyes"); err != nil {
		slog.Warn("github: fix-label ack reaction failed", "component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
	}

	sha := p.PullRequest.Head.SHA
	checks, err := e.failingChecks(ctx, ri.Owner, ri.Name, sha)
	if err != nil {
		slog.Warn("github: fix-label check fetch failed; the flag stays armed for the next CI event",
			"component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	if len(checks) == 0 {
		return // nothing failing right now - armed and waiting for the next CI failure
	}
	e.beginFix(ctx, ri, number, sha,
		fmt.Sprintf("@%s asked me (via the `%s` label) to fix this pull request's currently-failing checks.", p.Sender.Login, e.labels.Fix),
		renderFailingChecks(checks))
}

// beginFix persists the attempt BEFORE dispatch (a crash mid-run must never
// leave the guard unrecorded), then dispatches a fix run on the PR's existing
// session - the shared tail of both the automatic (workflow_run) and explicit
// (labeled) fix paths.
func (e *Extension) beginFix(ctx context.Context, ri repoInfo, number int, sha, intro, checksText string) {
	sessionID := fmt.Sprintf("github-%s-%s-%d", ri.Owner, ri.Name, number)
	if err := e.store.SetGithubFixState(ctx, store.GithubFixState{ChatID: sessionID, LastSHA: sha}); err != nil {
		slog.Error("github: fix-state persist failed; refusing to run without a durable bound",
			"component", "github", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}

	// Continue the PR's existing session under the identity it was written
	// with - session reuse is the point of this feature (#254).
	login := e.store.SessionUserForChat(ctx, sessionID)
	synthetic := issueCommentPayload{Action: "created"}
	synthetic.Issue.Number = number
	synthetic.Issue.PullRequest = &struct{}{}
	synthetic.Comment.User.Login = login
	synthetic.Repository.Name = ri.Name
	synthetic.Repository.Owner.Login = ri.Owner
	synthetic.Repository.CloneURL = ri.CloneURL
	synthetic.Repository.DefaultBranch = ri.DefaultBranch
	synthetic.Installation.ID = ri.InstallationID
	// isLabelTrigger stays false: a fix continues the PR's session, it never resets it.

	e.dispatch(synthetic, fixTask(intro, checksText))
}

// authoredByQuack reports whether owner/repo#number's PR was opened by quack
// itself - the "authorship IS the flag" check (#656): PR participation
// (fixing its own CI, addressing review findings - see engageOwnPRReview)
// needs no label on a PR quack authored.
func (e *Extension) authoredByQuack(ctx context.Context, owner, repo string, number int) (bool, error) {
	author, err := e.app.prAuthor(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	bot, err := e.app.botLogin(ctx)
	if err != nil {
		return false, err
	}
	return author == bot, nil
}

// commitAuthoredByQuack reports whether a commit was made by quack itself -
// see tools.GitCommitAuthorEmail. Used by autoHeal's one-attempt guard: the
// failing commit's actual author, not remembered state, is the source of
// truth for "was this CI failure my own fix's fault".
func (e *Extension) commitAuthoredByQuack(ctx context.Context, owner, repo, sha string) (bool, error) {
	email, err := e.app.commitAuthorEmail(ctx, owner, repo, sha)
	if err != nil {
		return false, err
	}
	return email == tools.GitCommitAuthorEmail, nil
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
