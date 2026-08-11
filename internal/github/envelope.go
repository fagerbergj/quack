package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/vetting"
)

// Trigger envelope (#659, #666): GitHub's own JSON filtered by drop-list, never renamed.

const seedCap = 32 * 1024

// permissionsText renders the run's allowed delivery kinds as the envelope's
// <permissions> vocabulary - the same closed vocabulary (pull_request,
// review, comment) staged delivery and the trust gate's allowlist use, so the
// envelope and enforcement can never name two different things.
func permissionsText(allowedKinds []string) string {
	return strings.Join(allowedKinds, ", ")
}

// dropField reports keys dropped from filtered event/comment JSON (node_id, *_url, avatar_url, reactions, performed_via_github_app).
func dropField(key string) bool {
	switch key {
	case "node_id", "avatar_url", "reactions", "performed_via_github_app", "url":
		return true
	}
	return strings.HasSuffix(key, "_url")
}

// filterGitHubJSON decodes raw and re-marshals with dropField's keys removed. "{}" on failure.
func filterGitHubJSON(raw []byte) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(filterJSONValue(v))
	if err != nil {
		return "{}"
	}
	return string(out)
}

func filterJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if dropField(k) {
				continue
			}
			out[k] = filterJSONValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = filterJSONValue(val)
		}
		return out
	default:
		return v
	}
}

// seededComment is a comment's four-field seed (#666): id, created_at, user.login, body.
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

func seededCommentsJSON(cs []snapshotComment) string {
	out := make([]seededComment, 0, len(cs))
	for _, c := range cs {
		out = append(out, toSeededComment(c))
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

// commentsBlock renders seed (full) or delta (new + edited + deleted) — uses diffSnapshots, never GitHub's ?since=.
func commentsBlock(gh githubContext, excludeCommentID int64) string {
	if gh.delta == nil {
		visible := visibleComments(gh.snap.Comments, excludeCommentID)
		return fmt.Sprintf("<comments count=\"%d\">%s</comments>\n", len(visible), seededCommentsJSON(visible))
	}
	d := gh.delta
	type group = struct {
		status string
		cs     []snapshotComment
	}
	body := seededCommentsWithStatus(
		group{"new", d.CommentsAdded},
		group{"edited", d.CommentsEdited},
		group{"deleted", d.CommentsDeleted},
	)
	return fmt.Sprintf("<comments new=\"%d\" edited=\"%d\" deleted=\"%d\">%s</comments>\n",
		len(d.CommentsAdded), len(d.CommentsEdited), len(d.CommentsDeleted), body)
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

// truncatedText caps at seedCap bytes with a plain-text note naming the full file.
func truncatedText(s, fullFile string) string {
	if len(s) <= seedCap {
		return s
	}
	return fmt.Sprintf("[TRUNCATED: full text is %d bytes; showing the first %d. Full text: %s]\n\n%s",
		len(s), seedCap, fullFile, s[:seedCap])
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

func eventBlock(p issueCommentPayload) string {
	name := p.eventName
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("<event name=%q>%s</event>\n", name, filterGitHubJSON(p.rawEvent))
}

// ContextFile names one file WriteContextDir wrote and the endpoint that produced it.
type ContextFile struct {
	Name     string
	Endpoint string
}

func contextFileEndpoint(name, owner, repo string, number int, checkSHA string) string {
	base := fmt.Sprintf("/repos/%s/%s", owner, repo)
	switch {
	case name == "issue.json":
		return fmt.Sprintf("GET %s/issues/%d", base, number)
	case name == "comments.json":
		return fmt.Sprintf("GET %s/issues/%d/comments", base, number)
	case name == "timeline.json":
		return fmt.Sprintf("GET %s/issues/%d/timeline", base, number)
	case name == "pull.json":
		return fmt.Sprintf("GET %s/pulls/%d", base, number)
	case name == "files.json":
		return fmt.Sprintf("GET %s/pulls/%d/files", base, number)
	case name == "commits.json":
		return fmt.Sprintf("GET %s/pulls/%d/commits", base, number)
	case name == "reviews.json":
		return fmt.Sprintf("GET %s/pulls/%d/reviews", base, number)
	case name == "review-comments.json":
		return fmt.Sprintf("GET %s/pulls/%d/comments", base, number)
	case name == "check-runs.json":
		return fmt.Sprintf("GET %s/commits/%s/check-runs", base, checkSHA)
	case strings.HasPrefix(name, "annotations-"):
		return fmt.Sprintf("GET %s/check-runs/{id}/annotations", base)
	case strings.HasPrefix(name, "linked-issue-") && strings.HasSuffix(name, "-comments.json"):
		n := strings.TrimSuffix(strings.TrimPrefix(name, "linked-issue-"), "-comments.json")
		return fmt.Sprintf("GET %s/issues/%s/comments", base, n)
	case strings.HasPrefix(name, "linked-issue-"):
		n := strings.TrimSuffix(strings.TrimPrefix(name, "linked-issue-"), ".json")
		return fmt.Sprintf("GET %s/issues/%s", base, n)
	default:
		return "GET (unknown endpoint)"
	}
}

func contextDirFiles(dir, owner, repo string, number int, checkSHA string) []ContextFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)
	out := make([]ContextFile, 0, len(names))
	for _, n := range names {
		out = append(out, ContextFile{Name: n, Endpoint: contextFileEndpoint(n, owner, repo, number, checkSHA)})
	}
	return out
}

func contextBlock(dir string, files []ContextFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<context dir=%q>\n", filepath.ToSlash(dir))
	for _, f := range files {
		fmt.Fprintf(&b, "  <file name=%q>%s</file>\n", f.Name, f.Endpoint)
	}
	b.WriteString("</context>\n")
	return b.String()
}

// buildEnvelope builds the trigger envelope: permissions, deliverable, ask, comments, changed_files, event, context.
func (e *Extension) buildEnvelope(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, ctxDir string, ctxFiles []ContextFile) string {
	isPR := p.Issue.PullRequest != nil
	deliverable := e.deliverableText(ctx, p, task, gh, allowedKinds, isPR)

	var b strings.Builder
	fmt.Fprintf(&b, "<permissions>%s</permissions>\n", permissionsText(allowedKinds))
	fmt.Fprintf(&b, "<deliverable>%s</deliverable>\n", deliverable)
	b.WriteString(askBlock(p, gh, isPR))
	b.WriteString(commentsBlock(gh, p.Comment.ID))
	if isPR {
		b.WriteString(changedFilesBlock(gh.snap))
	}
	b.WriteString(eventBlock(p))
	if ctxDir != "" {
		b.WriteString(contextBlock(ctxDir, ctxFiles))
	}
	return b.String()
}

// buildWorkerAsk is the consumer split for nodes (#664): ask-only text, never orchestrator-level evidence.
func (e *Extension) buildWorkerAsk(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, ctxDir string) string {
	isPR := p.Issue.PullRequest != nil
	deliverable := e.deliverableText(ctx, p, task, gh, allowedKinds, isPR)

	var b strings.Builder
	fmt.Fprintf(&b, "<permissions>%s</permissions>\n", permissionsText(allowedKinds))
	fmt.Fprintf(&b, "<deliverable>%s</deliverable>\n", deliverable)
	b.WriteString(askBlock(p, gh, isPR))
	b.WriteString(commentsBlock(gh, p.Comment.ID))
	if ctxDir != "" {
		fmt.Fprintf(&b, "<context dir=%q>Evidence for your task - diffs, CI annotations, review threads, linked issues - lives here as GitHub's own JSON, one file per endpoint. Read what your task needs.</context>\n",
			filepath.ToSlash(ctxDir))
	}
	return b.String()
}

// reviewDeliverableText scopes a review to commits not seen before (#459 §5).
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

// deliverableText classifies the run and states what it produces. Applies classifier or falls back to ImplementationIntent.
func (e *Extension) deliverableText(ctx context.Context, p issueCommentPayload, task string, gh githubContext, allowedKinds []string, isPR bool) string {
	mentionIsWork := isPR && !p.isLabelTrigger && p.deliverableHint == ""

	// #760: a comment on a PR quack can already push to has its permission question
	// answered deterministically by computeGrant (labels/authorship) - the generic
	// WORK/CONVERSATIONAL gate below was being asked to re-derive that MAY, and misread a
	// message that both corrected something said earlier and asked for a specific change
	// (home-server#3). Go straight to what the comment is asking for, bounded by the grant.
	// mentionIsWork ⇒ PR-scoped, so "pull_request" here means push-to-this-PR.
	if mentionIsWork && slices.Contains(allowedKinds, "pull_request") {
		if kind, ok := e.classifyGrantedPRDeliverable(ctx, task, allowedKinds); ok {
			switch kind {
			case "commit":
				return "a commit addressing the requested change"
			case "review":
				return reviewDeliverableText(gh)
			}
		}
		return replyDeliverable
	}
	if mentionIsWork && !e.isWorkRequest(ctx, task) {
		return replyDeliverable
	}

	issueCommentDeliverable := "an answer to their message, posted to the issue as a comment - a revised plan if one is already under discussion"

	switch {
	case p.planOnly:
		return "a PLANNING-ONLY implementation plan: your ANSWER TEXT is the plan, posted to the issue verbatim."
	case !isPR && p.isLabelTrigger:
		return issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
	case !isPR && !p.isLabelTrigger:
		// #713: a comment can ask for implementation too - the label only bounds whether it's a legal answer.
		if kind, ok := e.classifyIssueDeliverableCached(ctx, p, task, allowedKinds); ok {
			if kind == "implement" {
				return issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
			}
			return issueCommentDeliverable
		}
		// Classifier unavailable/failed/unparseable: fall back to the wording heuristic, never straight to conversational.
		// !isPR ⇒ issue-scoped, so "pull_request" here means open-a-new-PR.
		if slices.Contains(allowedKinds, "pull_request") && vetting.ImplementationIntent(task) {
			return issueImplementDeliverable(e.labels.PartialFix, gh.snap.Labels, p.Issue.Number)
		}
		return issueCommentDeliverable
	case p.deliverableHint != "":
		return p.deliverableHint
	}

	if mentionIsWork {
		if _, ok := e.classifyPRDeliverable(ctx, task, allowedKinds); ok {
			return reviewDeliverableText(gh)
		}
	}
	if isPR && !vetting.ImplementationIntent(task) {
		return reviewDeliverableText(gh)
	}
	return "a commit addressing the requested change"
}
