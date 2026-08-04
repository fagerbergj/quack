// Trigger envelope (#659, #666): GitHub's own JSON, filtered by a drop-list -
// never renamed or flattened - plus the seeded ask and quack's two own
// concepts, permissions and deliverable. Replaces webhook.go's old prose
// builder wholesale (design: .quack/trigger-prompts-v2.md). The frontend
// parser (frontend/src/components/envelope.ts, #671) renders this exact
// shape - keep the two in sync.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/vetting"
)

// seedCap bounds a seeded block's byte size so a run can start at all:
// QUACK_COMPACTION_ENABLED defaults false and a worker's context_window is
// 65536 tokens, so a 500KB body inlined verbatim is a context-length 400 on
// the FIRST call - the run never starts. The ONE sanctioned truncation in the
// trigger path; it always points at the untruncated file in the #660 context
// directory.
const seedCap = 32 * 1024

// permissionsText renders a Grant as the envelope's <permissions> vocabulary -
// the SAME tokens Grant.allows checks against (internal/vetting/grant.go), so
// this text and enforcement can never name two different things.
func permissionsText(g vetting.Grant) string {
	var perms []string
	if g.JoinIssueConversation {
		perms = append(perms, "join_issue_conversation")
	}
	if g.OpenPR {
		perms = append(perms, "open_pr")
	}
	if g.PostReview {
		perms = append(perms, "post_review")
	}
	if g.JoinPRConversation {
		perms = append(perms, "join_pr_conversation")
	}
	if g.PushCommitsToPR {
		perms = append(perms, "push_commits_to_pr")
	}
	return strings.Join(perms, ", ")
}

// dropField reports whether key is dropped from a filtered event/comment -
// GitHub's own noise (node_id, the *_url family, avatar_url, reactions,
// performed_via_github_app), never a rename or a reshape. A drop-list needs
// no maintenance when GitHub adds a field.
func dropField(key string) bool {
	switch key {
	case "node_id", "avatar_url", "reactions", "performed_via_github_app", "url":
		return true
	}
	return strings.HasSuffix(key, "_url")
}

// filterGitHubJSON decodes raw (a GitHub webhook payload) and re-marshals it
// with dropField's keys removed at every nesting level - filtering, never
// renaming or flattening, so a field the model has seen a million times in
// training keeps its GitHub shape and meaning. "{}" for empty/unparseable
// input - the event block degrades rather than breaking the whole envelope.
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

// filterJSONValue recurses through a decoded JSON value dropping fields
// dropField names, at every level - a top-level key and the SAME key nested
// three objects deep are both subject to the same rule.
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

// seededComment is a comment's four-field seed (#666): id, created_at,
// user.login, body - GitHub's own names and nesting, everything else
// dropped. Bodies stay whole; only the envelope around them goes. id is what
// lets a seeded comment be cross-referenced to comments.json (#660) and what
// an agent quotes when replying to a specific comment.
type seededComment struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Body string `json:"body"`
	// Status is quack's own field (explicitly namespaced, like
	// quack_reviewed_through), set only in delta mode: "new" | "edited" |
	// "deleted". Without it a deleted comment's body reads exactly like a live
	// one and the model has to infer which is which by counting positions
	// against the block's attributes - miscount once and a RETRACTED statement
	// is treated as current.
	Status string `json:"quack_status,omitempty"`
}

func toSeededComment(c snapshotComment) seededComment {
	var sc seededComment
	sc.ID = c.ID
	sc.CreatedAt = c.CreatedAt
	sc.User.Login = c.User
	sc.Body = c.Body
	return sc
}

// seededCommentsWithStatus marshals a delta's comments with each item marked
// new/edited/deleted, so the split is per-item rather than positional.
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

// seededCommentsJSON marshals cs to the seeded four-field shape - "[]" (never
// "null") for an empty slice, GitHub's own shape for an empty list.
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

// visibleComments drops a snapshot's minimized/hidden comments (see
// snapshotComment.Hidden) and, when set, the triggering comment itself -
// already quoted verbatim inside the event block's own comment.body, so
// seeding it again here would just duplicate it.
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

// commentsBlock renders session creation's full seed (every comment) or a
// later run's delta (new + edited + deleted since the last dispatch) -
// computed by the EXISTING snapshot diff (diffSnapshots), never GitHub's
// ?since=: that filters on updated_at, so a deletion produces no signal at
// all and an edit is indistinguishable from a new comment. Deleted comments
// are included in the array (their last-known id/created_at/user/body) so
// the deletion is actually visible, not just an opaque count.
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

// changedFilesBlock renders a PR's current file list - GitHub's own
// filename/additions/deletions/status shape (changedFile already matches
// pulls/{n}/files field-for-field), patches omitted (workers have the clone;
// files.json in the #660 context dir carries patches for anything without
// one).
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

// truncatedText caps s at seedCap bytes, marking the cut with a plain-text
// head note that names the untruncated file - the ONE sanctioned truncation
// in the trigger path. Plain text, not an XML attribute: the model reads
// prose, and this needs no frontend parser change to stay visible.
func truncatedText(s, fullFile string) string {
	if len(s) <= seedCap {
		return s
	}
	return fmt.Sprintf("[TRUNCATED: full text is %d bytes; showing the first %d. Full text: %s]\n\n%s",
		len(s), seedCap, fullFile, s[:seedCap])
}

// askBlock hoists the issue/PR title and description into their own seeded
// block (#666), dropped from the event JSON so it appears once. A changed
// title/description since the last dispatch is marked and quotes the actual
// change (GitHub's own changes.title.from on an edit event, or simply the
// fact of the change - see githubContext.delta).
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

// eventBlock renders the ORIGINATING webhook's own JSON, filtered but never
// reshaped - the model has seen these payloads a million times in training.
func eventBlock(p issueCommentPayload) string {
	name := p.eventName
	if name == "" {
		name = "unknown"
	}
	return fmt.Sprintf("<event name=%q>%s</event>\n", name, filterGitHubJSON(p.rawEvent))
}

// ContextFile names one file WriteContextDir wrote and the endpoint that
// produced it - contextDirFiles' return shape, rendered by contextBlock.
type ContextFile struct {
	Name     string
	Endpoint string
}

// contextFileEndpoint labels a #660 context-dir filename with the GitHub
// endpoint that produced it (display only - WriteContextDir already fetched
// it; this never makes a second call). Mirrors WriteContextDir's own naming
// exactly (internal/github/contextdir.go).
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

// contextDirFiles lists what WriteContextDir actually wrote to dir (fail-soft
// per file - a skipped fetch just isn't there), labelling each with the
// endpoint that produced it. nil (no <context> block at all) when dir can't
// be read.
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

// contextBlock renders the <context> block: the sibling directory's absolute
// path (sandbox grant covers it read-only - internal/acp's resolveNode) plus
// one <file> per endpoint dump.
func contextBlock(dir string, files []ContextFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<context dir=%q>\n", filepath.ToSlash(dir))
	for _, f := range files {
		fmt.Fprintf(&b, "  <file name=%q>%s</file>\n", f.Name, f.Endpoint)
	}
	b.WriteString("</context>\n")
	return b.String()
}

// buildEnvelope replaces the old prose builder wholesale (#659/#666):
// permissions, deliverable, the hoisted ask, comments, changed_files (PR
// runs), the filtered raw event, and the context directory - GitHub's own
// JSON, filtered by a drop-list, never reshaped. grant was already computed
// once at dispatch (#657/#662) and is carried here as INFORMATION only -
// enforcement lives at commitDelivery, never here (permissions ⇒ prompt
// text, not a second gate).
func (e *Extension) buildEnvelope(ctx context.Context, p issueCommentPayload, task string, gh githubContext, grant vetting.Grant, ctxDir string, ctxFiles []ContextFile) string {
	isPR := p.Issue.PullRequest != nil
	deliverable := e.deliverableText(ctx, p, task, gh, isPR)

	var b strings.Builder
	fmt.Fprintf(&b, "<permissions>%s</permissions>\n", permissionsText(grant))
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

// buildWorkerAsk is the consumer split's other half (#664): what a plan's
// NODES get as their BACKGROUND (dag.Plan.WorkerBackground), in place of
// buildEnvelope's full output. The ask is inlined whole here too - the split
// applies to EVIDENCE only - but changed_files, the raw event, and the
// context directory's per-file listing are all left out: a node reads its
// OWN slice (the files/check/thread its task names) off the clone and the
// context directory itself, guided by its task text, never the orchestrator's
// planning-scale summaries. ctxDir, when set, is still named so a node knows
// evidence lives there to read on demand.
func (e *Extension) buildWorkerAsk(ctx context.Context, p issueCommentPayload, task string, gh githubContext, grant vetting.Grant, ctxDir string) string {
	isPR := p.Issue.PullRequest != nil
	deliverable := e.deliverableText(ctx, p, task, gh, isPR)

	var b strings.Builder
	fmt.Fprintf(&b, "<permissions>%s</permissions>\n", permissionsText(grant))
	fmt.Fprintf(&b, "<deliverable>%s</deliverable>\n", deliverable)
	b.WriteString(askBlock(p, gh, isPR))
	b.WriteString(commentsBlock(gh, p.Comment.ID))
	if ctxDir != "" {
		fmt.Fprintf(&b, "<context dir=%q>Evidence for your task - diffs, CI annotations, review threads, linked issues - lives here as GitHub's own JSON, one file per endpoint. Read what your task needs.</context>\n",
			filepath.ToSlash(ctxDir))
	}
	return b.String()
}

// deliverableText classifies the run exactly as the old prose builder did -
// planOnly / label-triggered implement / a plain issue mention / a PR
// conversational follow-up (isWorkRequest, intent.go) / reviewOnly
// (vetting.ImplementationIntent, the SAME discriminator the planner backstop
// uses) - and states the ONE thing this run is asked to produce. Permissions
// and the ask+event blocks carry everything else; this never repeats them.
func (e *Extension) deliverableText(ctx context.Context, p issueCommentPayload, task string, gh githubContext, isPR bool) string {
	// A mention on a PR defaults to CONVERSATIONAL unless the classifier calls
	// it a work request - fails safe to conversational, since a wrong WORK
	// verdict re-reviews and discards a written reply. deliverableHint == ""
	// excludes a SYNTHETIC system trigger (CI auto-heal, own-PR
	// review-response): those never had a human mention to classify at all,
	// and are unambiguously work by construction, same as a label trigger.
	if isPR && !p.isLabelTrigger && p.deliverableHint == "" && !e.isWorkRequest(ctx, task) {
		return "a reply to their message, posted as a comment - no new work unless they explicitly ask for it"
	}

	reviewOnly := isPR && !vetting.ImplementationIntent(task)
	switch {
	case p.planOnly:
		// PLANNING-ONLY: read the repo, change nothing, deliver nothing else to
		// GitHub. The two clauses below are both incident scars (#569, and a
		// stale-dependency-version assertion) with no other home under the
		// envelope - #662a (moving constant instructions into bundle prompts)
		// is a separate, later PR.
		return "a PLANNING-ONLY implementation plan: your ANSWER TEXT is the plan, posted to the issue verbatim - never a file path (any file this run writes is discarded with its working directory when the run ends, so a path reference is a dangling pointer to nothing). Do not assert a dependency version, action tag, or API detail from memory as if it were current - say \"the current stable X\" rather than naming a version you have not verified this session."
	case !isPR && p.isLabelTrigger:
		// quack:implement applied to the issue.
		if hasPartialFix(e.labels.PartialFix, gh.snap.Labels) {
			return "a pull request implementing the changes, without a Closes keyword (this is a partial fix)"
		}
		return fmt.Sprintf("a pull request implementing the approved plan, body containing `Closes #%d`", p.Issue.Number)
	case !isPR && !p.isLabelTrigger:
		return "an answer to their message, posted to the issue as a comment - a revised plan if one is already under discussion"
	case p.deliverableHint != "":
		// A SYNTHETIC system trigger's deliverable is fixed by which webhook
		// dispatched it (CI auto-heal, own-PR review-response) - authoritative,
		// ahead of the fuzzy review/implement wording heuristic below, which a
		// "read the review comments... make the fix... commit" sentence can
		// misclassify as review-only (its impl verbs aren't clause-initial).
		return p.deliverableHint
	case reviewOnly:
		if gh.newCommits != nil {
			// A review has already been delivered on this chat (reviewScope) -
			// scope the ask to what's new since then, by content (patch-id),
			// never off the general context delta (that would under-scope
			// whenever a conversational dispatch landed between two reviews).
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
	default:
		return "a commit addressing the requested change"
	}
}
