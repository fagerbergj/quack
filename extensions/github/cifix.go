// PR self-heal: on a quack:fix-labeled or quack-authored PR, a CI failure dispatches one fix.
// A failure on quack's own fix commit stops the loop and waits for a human.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// gitCommitAuthorEmail mirrors quack's internal/tools.GitCommitAuthorEmail; duplicated rather than
// adding a Host capability for one constant.
const gitCommitAuthorEmail = "agent@quack.local"

// fixContextTimeout bounds pre-dispatch API phase (labels, check runs, annotations).
const fixContextTimeout = 2 * time.Minute

// workflowRunPayload: subset of GitHub's workflow_run webhook. Fork PRs never auto-healed.
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
	Repository   ghRepository   `json:"repository"`
	Installation ghInstallation `json:"installation"`
}

// repoInfo: repository/installation identity for fix dispatch.
type repoInfo struct {
	Owner, Name, CloneURL, DefaultBranch string
	InstallationID                       int64
}

func repoInfoOf(r ghRepository, inst ghInstallation) repoInfo {
	return repoInfo{r.Owner.Login, r.Name, r.CloneURL, r.DefaultBranch, inst.ID}
}

// synthetic shapes a non-webhook trigger as an issueCommentPayload so dispatch handles it like a mention.
func (ri repoInfo) synthetic(number int, login string) issueCommentPayload {
	p := issueCommentPayload{Action: "created"}
	p.Issue.Number = number
	p.Comment.User.Login = login
	p.Repository.Name, p.Repository.Owner.Login = ri.Name, ri.Owner
	p.Repository.CloneURL, p.Repository.DefaultBranch = ri.CloneURL, ri.DefaultBranch
	p.Installation.ID = ri.InstallationID
	return p
}

// autoReview is the label-trigger auto-review run on a PR, never a mention.
func (ri repoInfo) autoReview(number int, title string, rawEvent []byte, eventName string) issueCommentPayload {
	p := ri.synthetic(number, autoReviewUser)
	p.Issue.Title = title
	p.Issue.PullRequest = &struct{}{}
	p.isLabelTrigger = true
	p.rawEvent = json.RawMessage(rawEvent)
	p.eventName = eventName
	return p
}

// handleWorkflowRun: CI auto-heal trigger. Not bot-sender-gated (quack's own fix push re-triggers CI).
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
	if len(p.WorkflowRun.PullRequests) == 0 {
		w.WriteHeader(http.StatusOK) // branch push with no open same-repo PR, or a fork PR
		return
	}
	e.host.Log.Info("github webhook received",
		"repo", p.Repository.Owner.Login+"/"+p.Repository.Name, "workflow", p.WorkflowRun.Name,
		"conclusion", p.WorkflowRun.Conclusion, "head_sha", p.WorkflowRun.HeadSHA,
		"installation", p.Installation.ID)
	for _, pr := range p.WorkflowRun.PullRequests {
		e.spawn(func() { e.autoHeal(p, pr.Number, body) })
	}
	w.WriteHeader(http.StatusAccepted)
}

// autoHeal gates (eligibility, per-commit dedup, one-attempt guard) then dispatches the fix. Every
// store/API failure fails closed: an unbounded fix loop is worse than a missed heal.
func (e *Extension) autoHeal(p workflowRunPayload, number int, rawBody []byte) {
	ri := repoInfoOf(p.Repository, p.Installation)
	chatID := globalChatID(issueSessionID(ri.Owner, ri.Name, number))
	sha := p.WorkflowRun.HeadSHA

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	_, _, _, labels, _, err := e.app.issueMeta(ctx, ri.Owner, ri.Name, number)
	if err != nil {
		e.host.Log.Warn("github: auto-heal eligibility check failed; skipping", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	eligible := slices.Contains(labels, e.labels.Fix)
	if !eligible {
		// Authorship is the flag: quack fixes its own PR's CI with no label.
		authored, aerr := e.authoredByQuack(ctx, ri.Owner, ri.Name, number)
		if aerr != nil {
			e.host.Log.Warn("github: auto-heal authorship check failed; skipping", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", aerr)
			return
		}
		eligible = authored
	}
	if !eligible {
		return // no quack:fix label and not quack's own PR - never auto-heal
	}

	// GetFixState through every SetFixState (here and in beginFix) is one atomic claim, or two
	// workflow_run deliveries for the same head both pass the read and both dispatch.
	unlock := e.mergeMu.Lock(chatID)
	defer unlock()

	st, err := e.store.GetFixState(ctx, chatID)
	if err != nil {
		e.host.Log.Warn("github: auto-heal state read failed; skipping", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	if st != nil && st.LastSHA == sha {
		// Another failing workflow on a commit already handled (CI usually
		// runs several) - one heal per head commit.
		e.host.Log.Info("github: auto-heal already handled this head commit; skipping", "repo", ri.Owner+"/"+ri.Name, "pr", number, "sha", sha)
		return
	}

	// One-attempt guard, only after a prior fix (st != nil): on quack's own PR every commit is quack's,
	// including the first, so an unconditional authorship check would never attempt a fix at all.
	var ownCommit bool
	if st != nil {
		var cerr error
		ownCommit, cerr = e.commitAuthoredByQuack(ctx, ri.Owner, ri.Name, sha)
		if cerr != nil {
			e.host.Log.Warn("github: auto-heal could not verify the failing commit's author; skipping rather than risk a fix loop",
				"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", cerr)
			return
		}
	}

	checksText, checks := e.failingChecksText(ctx, ri.Owner, ri.Name, sha, p.WorkflowRun.Name, p.WorkflowRun.HTMLURL)
	e.observeCIFailure(ctx, chatID, ri, number, sha, checks)

	if ownCommit {
		if err := e.store.SetFixState(ctx, FixState{ChatID: chatID, LastSHA: sha, Stopped: true}); err != nil {
			e.host.Log.Warn("github: auto-heal stop-state write failed", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		}
		// The one comment for this trigger: no run follows, so nothing else will say why.
		e.comment(ctx, ri.Owner, ri.Name, number, fmt.Sprintf("Auto-heal stopped: CI still fails on quack's own fix `%s`; no second attempt. Push a fix or ask with guidance.\n\n%s",
			shortSHA(sha), checksText))
		return
	}

	e.host.Log.Info("github: auto-heal dispatching fix run", "repo", ri.Owner+"/"+ri.Name, "pr", number, "sha", sha)
	e.ackIssue(ri.Owner, ri.Name, number) // the fix run's push (or answer) is the outcome
	e.beginFix(ctx, ri, number, sha, "CI is failing on this pull request.", checksText, rawBody, "workflow_run.completed")
}

// fixLabelApplied re-arms auto-heal (clearing any prior stop) and fixes now only if CI is currently
// failing; on a green PR it does nothing but stay armed.
func (e *Extension) fixLabelApplied(p pullRequestPayload, rawBody []byte) {
	ri := repoInfoOf(p.Repository, p.Installation)
	number := p.Number
	chatID := globalChatID(issueSessionID(ri.Owner, ri.Name, number))

	ctx, cancel := context.WithTimeout(context.Background(), fixContextTimeout)
	defer cancel()

	if err := e.store.DeleteFixState(ctx, chatID); err != nil {
		e.host.Log.Warn("github: fix-state reset failed", "repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
	}
	e.ackIssue(ri.Owner, ri.Name, number)

	sha := p.PullRequest.Head.SHA
	checks, err := e.failingChecks(ctx, ri.Owner, ri.Name, sha)
	if err != nil {
		e.host.Log.Warn("github: fix-label check fetch failed; the flag stays armed for the next CI event",
			"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}
	if len(checks) == 0 {
		return // nothing failing right now - armed and waiting for the next CI failure
	}
	e.observeCIFailure(ctx, chatID, ri, number, sha, checks)
	e.beginFix(ctx, ri, number, sha,
		fmt.Sprintf("The `%s` label asks for this pull request's currently-failing checks to be fixed.", e.labels.Fix),
		renderFailingChecks(checks), rawBody, "pull_request.labeled")
}

// beginFix persists the attempt before dispatch, so a crash mid-run never leaves the guard unrecorded,
// then dispatches the fix on the PR's existing session. sha also scopes the "check-runs" artifact.
func (e *Extension) beginFix(ctx context.Context, ri repoInfo, number int, sha, intro, checksText string, rawBody []byte, eventName string) {
	chatID := globalChatID(issueSessionID(ri.Owner, ri.Name, number))
	if err := e.store.SetFixState(ctx, FixState{ChatID: chatID, LastSHA: sha}); err != nil {
		e.host.Log.Error("github: fix-state persist failed; refusing to run without a durable bound",
			"repo", ri.Owner+"/"+ri.Name, "pr", number, "err", err)
		return
	}

	// Continue the PR's existing session under the identity it was written with.
	login := ""
	if e.host.ChatUser != nil {
		login, _ = e.host.ChatUser(chatID)
	}
	synthetic := ri.synthetic(number, login)
	synthetic.Issue.PullRequest = &struct{}{}
	// isLabelTrigger stays false: a fix continues the PR's session, it never resets it.
	synthetic.rawEvent = json.RawMessage(rawBody)
	synthetic.eventName = eventName
	synthetic.checkSHA = sha
	synthetic.deliverableHint = "commits on this PR's head branch that make the failing checks pass"

	e.dispatch(synthetic, fixTask(intro, checksText))
}

// authoredByQuack reports whether quack opened the PR; on its own PR, fixing CI and addressing
// review findings need no label.
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

// commitAuthoredByQuack reports whether quack made the commit: the one-attempt guard trusts the
// failing commit's real author, not remembered state.
func (e *Extension) commitAuthoredByQuack(ctx context.Context, owner, repo, sha string) (bool, error) {
	email, err := e.app.commitAuthorEmail(ctx, owner, repo, sha)
	if err != nil {
		return false, err
	}
	return email == gitCommitAuthorEmail, nil
}

// fixTask is the classification signal and title fallback, never rendered into the envelope. Its
// implement-and-deliver wording keeps implementationIntent routing it as implement if the hint is unset.
func fixTask(intro, checksText string) string {
	var b strings.Builder
	b.WriteString(intro)
	b.WriteString(" Diagnose the failures below and fix them IN PLACE: make the smallest change that gets the checks green, run the repo's own checks locally to verify, and commit your work on the PR's existing head branch (already checked out for you). Do not start a new branch and do not open a new pull request - your commit updates THIS pull request.\n\nCI builds the MERGE of this branch with its base, not the branch alone (see the <ci_ref> instruction you were given) - merge the base in first and diagnose against that merged state, or you will miss a merge-only failure and risk delivering a no-op.\n\nFailing checks:\n")
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

// failingChecks returns the commit's failed check runs with annotations, which carry the actual
// error lines without downloading log archives.
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
			e.host.Log.Warn("github: check annotations fetch failed; continuing without them",
				"repo", owner+"/"+repo, "check", r.Name, "err", aerr)
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

// renderOneCheck renders one failing check's summary and annotations.
func renderOneCheck(c failingCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (%s)\n", c.Name, c.URL)
	if c.Summary != "" {
		fmt.Fprintf(&b, "  %s\n", truncate(c.Summary, 600))
	}
	for _, a := range c.Annotations {
		fmt.Fprintf(&b, "  %s\n", a)
	}
	return b.String()
}

// renderFailingChecks renders the failing checks as prompt context, bounded.
func renderFailingChecks(checks []failingCheck) string {
	var b strings.Builder
	for i, c := range checks {
		if i == maxFailingChecks {
			fmt.Fprintf(&b, "… and %d more failing checks\n", len(checks)-maxFailingChecks)
			break
		}
		b.WriteString(renderOneCheck(c))
	}
	return truncate(b.String(), maxChecksContextRunes)
}

// ciChecksForNodes renders each check in isolation so a node matched to one check never
// inherits another's annotations.
func ciChecksForNodes(checks []failingCheck) []sdk.NamedContext {
	out := make([]sdk.NamedContext, 0, len(checks))
	for _, c := range checks {
		out = append(out, sdk.NamedContext{Name: c.Name, Detail: truncate(renderOneCheck(c), maxChecksContextRunes)})
	}
	return out
}

// failingChecksText falls back to the workflow's name and URL when the checks API is unreadable
// or lags the workflow_run event.
func (e *Extension) failingChecksText(ctx context.Context, owner, repo, sha, workflowName, workflowURL string) (string, []failingCheck) {
	checks, err := e.failingChecks(ctx, owner, repo, sha)
	if err != nil {
		e.host.Log.Warn("github: failing-check fetch failed; using the workflow reference only", "repo", owner+"/"+repo, "sha", sha, "err", err)
	}
	if len(checks) == 0 {
		return fmt.Sprintf("- workflow %q failed on commit %s: %s (no further check detail was readable - inspect the repo's CI config and run its checks locally to reproduce)\n", workflowName, shortSHA(sha), workflowURL), nil
	}
	return renderFailingChecks(checks), checks
}
