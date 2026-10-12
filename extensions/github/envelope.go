package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const seedCap = 32 * 1024

// permissionsText renders allowed delivery kinds in the same closed vocabulary
// staged delivery and the trust gate's allowlist use, so envelope and enforcement agree.
func permissionsText(allowedKinds []string) string {
	return strings.Join(allowedKinds, ", ")
}

// seededComment is a comment's four-field seed: id, created_at, user.login, body.
type seededComment struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Body   string `json:"body"`
	Status string `json:"quack_status,omitempty"` // "new" | "edited" | "deleted" in delta mode
}

func toSeededComment(c snapshotComment) seededComment {
	var sc seededComment
	sc.ID = c.ID
	sc.CreatedAt = c.CreatedAt
	sc.User.Login = c.User
	sc.Body = c.Body
	return sc
}

func seededCommentsWithStatus(groups ...struct {
	status string
	cs     []snapshotComment
}) string {
	out := []seededComment{}
	for _, g := range groups {
		for _, c := range g.cs {
			sc := toSeededComment(c)
			sc.Status = g.status
			out = append(out, sc)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func visibleComments(cs []snapshotComment, excludeCommentID int64) []snapshotComment {
	out := make([]snapshotComment, 0, len(cs))
	for _, c := range cs {
		if c.Hidden || (excludeCommentID != 0 && c.ID == excludeCommentID) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// commentsBlock renders seed (full) or delta (new + edited + deleted) from diffSnapshots, never GitHub's ?since=.
func commentsBlock(gh githubContext, excludeCommentID int64) string {
	type group = struct {
		status string
		cs     []snapshotComment
	}
	if gh.delta == nil {
		visible := visibleComments(gh.snap.Comments, excludeCommentID)
		return fmt.Sprintf("<comments count=\"%d\">%s</comments>\n", len(visible), seededCommentsWithStatus(group{"", visible}))
	}
	d := gh.delta
	body := seededCommentsWithStatus(
		group{"new", d.CommentsAdded},
		group{"edited", d.CommentsEdited},
		group{"deleted", d.CommentsDeleted},
	)
	return fmt.Sprintf("<comments new=\"%d\" edited=\"%d\" deleted=\"%d\">%s</comments>\n",
		len(d.CommentsAdded), len(d.CommentsEdited), len(d.CommentsDeleted), body)
}

// maxEnvelopeChecks caps the rendered <checks> lines - a PR with a huge
// matrix build still gets a bounded envelope, with a note that it was cut.
const maxEnvelopeChecks = 20

// checksBlock renders head-commit check runs as compact status lines plus a summary;
// the "check-runs" input artifact holds the full JSON.
func checksBlock(checks []checkRunView) string {
	if len(checks) == 0 {
		return ""
	}
	var failing, pending, passing int
	for _, c := range checks {
		switch {
		case c.Status != "completed":
			pending++
		case c.Conclusion == "failure" || c.Conclusion == "timed_out":
			failing++
		default:
			passing++
		}
	}
	shown := checks
	truncated := len(shown) > maxEnvelopeChecks
	if truncated {
		shown = shown[:maxEnvelopeChecks]
	}
	var b strings.Builder
	summary := fmt.Sprintf("%d failing, %d pending, %d passing", failing, pending, passing)
	fmt.Fprintf(&b, "<checks count=\"%d\" summary=%q", len(checks), summary)
	if truncated {
		fmt.Fprintf(&b, " truncated=\"showing %d of %d\"", len(shown), len(checks))
	}
	b.WriteString(">\n")
	for _, c := range shown {
		if c.Status == "completed" {
			fmt.Fprintf(&b, "%s: completed %s\n", c.Name, c.Conclusion)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", c.Name, c.Status)
		}
		for _, w := range c.Why {
			fmt.Fprintf(&b, "  why: %s\n", w)
		}
	}
	b.WriteString("</checks>\n")
	return b.String()
}

func changedFilesBlock(snap Snapshot) string {
	var additions, deletions int
	for _, f := range snap.Files {
		additions += f.Additions
		deletions += f.Deletions
	}
	files := snap.Files
	if files == nil {
		files = []changedFile{}
	}
	b, err := json.Marshal(files)
	if err != nil {
		b = []byte("[]")
	}
	return fmt.Sprintf("<changed_files count=\"%d\" additions=\"%d\" deletions=\"%d\">%s</changed_files>\n",
		len(snap.Files), additions, deletions, string(b))
}

// truncatedText caps at seedCap bytes (on a rune boundary) with a note naming the full file.
func truncatedText(s, fullFile string) string {
	if len(s) <= seedCap {
		return s
	}
	cut := seedCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("[TRUNCATED: full text is %d bytes; showing the first %d. Full text: %s]\n\n%s",
		len(s), cut, fullFile, s[:cut])
}

func askBlock(p issueCommentPayload, gh githubContext, isPR bool) string {
	tag, fullFile := "issue", "issue.json"
	if isPR {
		tag, fullFile = "pull_request", "pull.json"
	}
	title := gh.snap.Title
	if gh.delta != nil && gh.delta.TitleChanged {
		title = fmt.Sprintf("%s (changed from %q)", title, gh.delta.OldTitle)
	}
	desc := truncatedText(gh.snap.Body, fullFile)
	if gh.delta != nil && gh.delta.BodyChanged {
		desc = "[description changed since your last look]\n\n" + desc
	}
	return fmt.Sprintf("<%s number=\"%d\">\n  <title>%s</title>\n  <description>%s</description>\n</%s>\n",
		tag, p.Issue.Number, title, desc, tag)
}

// compactEvent is the inline event: the fields prompts consume plus the trigger comment's body, which
// <comments> excludes so it isn't shipped twice. The full payload is the "event" input artifact.
type compactEvent struct {
	Action  string `json:"action"`
	Actor   string `json:"actor,omitempty"`
	Number  int    `json:"number,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Comment string `json:"comment,omitempty"`
}

func eventNote(p issueCommentPayload) string {
	name := p.eventName
	if name == "" {
		name = "unknown"
	}
	return name
}

func eventBlock(p issueCommentPayload) string {
	ce := compactEvent{Action: p.Action, Actor: p.Comment.User.Login, Number: p.Issue.Number, HeadSHA: p.checkSHA}
	if p.Comment.ID != 0 {
		ce.Comment = p.Comment.Body
	}
	b, err := json.Marshal(ce)
	if err != nil {
		b = []byte("{}")
	}
	return fmt.Sprintf("<event name=%q>%s</event>\n", eventNote(p), string(b))
}

// artifactsManifestBlock lists each input artifact this dispatch wrote or reused, so a worker
// knows what read_artifact can fetch without paying for the content inline.
func artifactsManifestBlock(entries []artifactEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<artifacts>\n")
	for _, e := range entries {
		status := "unchanged"
		if e.Changed {
			status = "new"
		}
		fmt.Fprintf(&b, "  <artifact id=%q revision=\"%d\" status=%q>%s</artifact>\n", e.ID(), e.Revision, status, e.Note)
	}
	b.WriteString("</artifacts>\n")
	return b.String()
}

// buildEnvelope builds the orchestrator's trigger envelope.
func (e *Extension) buildEnvelope(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, manifest []artifactEntry) string {
	return e.envelope(ctx, p, task, gh, allowedKinds, manifest, true)
}

// buildWorkerAsk is the node-scoped ask: no changed_files or event evidence. Checks stay,
// since a review node with no CI status can approve red.
func (e *Extension) buildWorkerAsk(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, manifest []artifactEntry) string {
	return e.envelope(ctx, p, task, gh, allowedKinds, manifest, false)
}

func (e *Extension) envelope(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, manifest []artifactEntry, full bool) string {
	isPR := p.Issue.PullRequest != nil
	deliverable := e.deliverableText(ctx, p, task, gh, allowedKinds, isPR)

	var b strings.Builder
	fmt.Fprintf(&b, "<permissions>%s</permissions>\n", permissionsText(allowedKinds))
	fmt.Fprintf(&b, "<deliverable>%s</deliverable>\n", deliverable)
	if p.checkSHA != "" {
		b.WriteString(ciMergeRefBlock(p.checkSHA, setupBaseRef(p, gh)))
	}
	b.WriteString(askBlock(p, gh, isPR))
	b.WriteString(commentsBlock(gh, p.Comment.ID))
	if isPR && full {
		b.WriteString(changedFilesBlock(gh.snap))
	}
	if isPR {
		b.WriteString(checksBlock(gh.checks))
	}
	if full {
		b.WriteString(eventBlock(p))
	}
	b.WriteString(artifactsManifestBlock(manifest))
	return b.String()
}

// ciMergeRefBlock: Actions builds head merged with base, so a merge-only failure is invisible on the head
// branch alone. checkSHA is the head commit the checks report against; only ci_fix dispatch sets it.
func ciMergeRefBlock(checkSHA, baseRef string) string {
	return fmt.Sprintf("<ci_ref>GitHub Actions builds the MERGE of this branch with base %q, not the checked-out head branch by itself - the failing checks are reported against head commit %s, but that merge is the actual CI build target. A failure can exist ONLY in the merge (e.g. two independently-fine changes that conflict once combined) and stay invisible if you diagnose the head branch alone. Before diagnosing: run `git merge \"origin/%s\"` in the checked-out clone, then diagnose and fix against that MERGED state, including any merge conflicts or merge-only compile errors. If the merge introduces no changes and you cannot reproduce the reported failure, say so explicitly in your answer - never report the checks as passing or deliver a no-op commit.</ci_ref>\n",
		baseRef, shortSHA(checkSHA), baseRef)
}

// reviewDeliverableText scopes a review to commits not seen before.
func reviewDeliverableText(gh githubContext) string {
	if gh.newCommits != nil {
		if len(gh.newCommits) == 0 {
			return "a review of what is new since the last one - you have already looked at every commit currently on this pull request (by content - a rebase or force-push may have changed their SHAs without changing what they do); only respond to any new discussion"
		}
		shas := make([]string, 0, len(gh.newCommits))
		for _, c := range gh.newCommits {
			shas = append(shas, shortSHA(c.SHA))
		}
		return fmt.Sprintf("a review of what is new since the last one - Focus your review on what's NEW since you last looked: commit(s) not seen before: %s",
			strings.Join(shas, ", "))
	}
	return "a review with inline comments and a verdict"
}

// replyDeliverable: the <deliverable> text for a run whose only legal output is a reply.
const replyDeliverable = "a reply to their message, posted as a comment - no new work unless they explicitly ask for it"

const commitDeliverable = "a commit addressing the requested change"

// deliverableText is deliverable's text; it keeps the first kind a dispatch resolves for the intent point.
func (e *Extension) deliverableText(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, isPR bool) string {
	kind, text := e.deliverable(ctx, p, task, gh, allowedKinds, isPR)
	if p.deliverableKind != nil && *p.deliverableKind == "" {
		*p.deliverableKind = kind
	}
	return text
}

// deliverable classifies the run and states what it produces; ImplementationIntent is the classifier fallback.
func (e *Extension) deliverable(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, isPR bool) (kind, text string) {
	mentionIsWork := isPR && !p.isLabelTrigger && p.deliverableHint == ""
	if kind, s := e.mentionWorkText(ctx, mentionIsWork, p, task, gh, allowedKinds); s != "" {
		return kind, s
	}

	issueCommentDeliverable := "an answer to their message, posted to the issue as a comment - a revised plan if one is already under discussion"

	switch {
	case p.planOnly:
		return "plan", "a PLANNING-ONLY implementation plan: your ANSWER TEXT is the plan, posted to the issue verbatim."
	case !isPR && p.isLabelTrigger:
		return "pull_request", issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
	case !isPR && !p.isLabelTrigger:
		return e.issueDeliverableText(ctx, p, task, allowedKinds, gh, issueCommentDeliverable)
	case p.deliverableHint != "":
		return "", p.deliverableHint
	}

	// Without push permission a PR mention can only be a review, which the grant alone decides.
	if (mentionIsWork && slices.Contains(allowedKinds, "review")) || (isPR && !ImplementationIntent(task)) {
		return "review", reviewDeliverableText(gh)
	}
	return "commit", commitDeliverable
}

// mentionWorkText: on a PR quack can already push to, computeGrant answers the permission
// question deterministically instead of the generic gate re-deriving it.
func (e *Extension) mentionWorkText(ctx context.Context, mentionIsWork bool, p issueCommentPayload, task string, gh githubContext, allowedKinds []string) (kind, text string) {
	if !mentionIsWork {
		return "", ""
	}
	// mentionIsWork ⇒ PR-scoped, so "pull_request" here means push-to-this-PR.
	if slices.Contains(allowedKinds, "pull_request") {
		if kind, ok := e.classifyGrantedPRDeliverable(ctx, task, allowedKinds); ok {
			switch kind {
			case "commit":
				return "commit", commitDeliverable
			case "review":
				return "review", reviewDeliverableText(gh)
			}
		}
		return "reply", replyDeliverable
	}
	if !e.isWorkRequest(ctx, p, task) {
		return "reply", replyDeliverable
	}
	return "", ""
}

// issueDeliverableText: the classifier (with its "implement" quadrant) first, then the
// wording heuristic as fallback, never straight to conversational.
func (e *Extension) issueDeliverableText(ctx context.Context, p issueCommentPayload, task string, allowedKinds []string, gh githubContext, issueCommentDeliverable string) (kind, text string) {
	// A comment can ask for implementation too; the label only bounds whether it's a legal answer.
	if kind, ok := e.classifyIssueDeliverableCached(ctx, p, task, allowedKinds); ok {
		if kind == "implement" {
			return "pull_request", issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
		}
		return "reply", issueCommentDeliverable
	}
	// !isPR ⇒ issue-scoped, so "pull_request" here means open-a-new-PR.
	if slices.Contains(allowedKinds, "pull_request") && ImplementationIntent(task) {
		return "pull_request", issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
	}
	return "reply", issueCommentDeliverable
}
