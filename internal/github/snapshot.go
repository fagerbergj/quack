// Snapshot-and-diff session context (#459): on every dispatch quack fetches
// the FULL current GitHub state for the issue/PR, diffs it against the last
// snapshot stored for this session, and injects only the delta as the turn's
// message — replacing the per-event, cherry-picked assembly this file used to
// split across runMessage/gatherReviewContext/issueThreadContext (#457/#458's
// interim, which re-injected every comment on every run). GitHub stays the
// source of truth; the stored snapshot is a cache the session's own (trimmed,
// compacted) event log is never trusted to hold.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// snapshotComment is one comment as captured in a Snapshot.
type snapshotComment struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Body      string `json:"body"`
	User      string `json:"user"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Hidden marks a minimized comment (GraphQL isMinimized) — kept in the
	// snapshot but filtered out of the rendered context (renderSeedContext).
	// TODO(#459 follow-up): always false today. The REST API this file uses
	// doesn't expose isMinimized; wiring it needs a GraphQL fetch (query
	// shape: repository(owner,name){ issue(number){ comments(first:100){
	// nodes{ databaseId isMinimized } } } }, pullRequest(number) equivalent
	// for PR comments) — deliberately left as a seam (this field) rather than
	// half-building it, since the hidden-comment RETRIEVAL tool (native +
	// MCP, for the ACP agents) is a separate, larger piece of work.
	Hidden bool `json:"hidden,omitempty"`
}

// snapshotReview is one submitted PR review.
type snapshotReview struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        string `json:"user"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// snapshotReviewComment is one inline PR review comment.
type snapshotReviewComment struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Body        string `json:"body"`
	User        string `json:"user"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
	// Resolved marks a comment whose review thread is isResolved (GraphQL).
	// TODO(#459 follow-up): always false today — see snapshotComment.Hidden;
	// same deferred GraphQL wiring (query shape: pullRequest(number){
	// reviewThreads(first:100){ nodes{ isResolved comments(first:100){
	// nodes{ databaseId } } } } }, mapping each thread's databaseId comments
	// back to this list).
	Resolved bool `json:"resolved,omitempty"`
}

// snapshotCommit is one PR commit, identified by its rebase-stable patch-id
// (not its SHA — a rebase/force-push rewrites every SHA even when the actual
// change is unchanged; see gitPatchID and diffSnapshots).
type snapshotCommit struct {
	SHA     string `json:"sha"`
	PatchID string `json:"patch_id"`
	Message string `json:"message"`
}

// Snapshot is the full GitHub state for one issue/PR session, as of the last
// dispatch — the ground truth diffSnapshots compares against. Every field is
// stored whole; cherry-picking (which comments matter, which are stale)
// happens only when RENDERING context, never here.
type Snapshot struct {
	Title          string                  `json:"title"`
	Body           string                  `json:"body"`
	State          string                  `json:"state"`
	Labels         []string                `json:"labels,omitempty"`
	Comments       []snapshotComment       `json:"comments,omitempty"`
	IsPR           bool                    `json:"is_pr,omitempty"`
	HeadRef        string                  `json:"head_ref,omitempty"`
	HeadSHA        string                  `json:"head_sha,omitempty"`
	BaseRef        string                  `json:"base_ref,omitempty"`
	Reviews        []snapshotReview        `json:"reviews,omitempty"`
	ReviewComments []snapshotReviewComment `json:"review_comments,omitempty"`
	Commits        []snapshotCommit        `json:"commits,omitempty"`
	Files          []changedFile           `json:"files,omitempty"`
}

// fetchSnapshot fetches the CURRENT full GitHub state for one issue/PR — the
// same call shape every dispatch makes, issue or PR, work request or
// conversational follow-up (#459's "one unified path"). Every sub-fetch past
// the required title/body/state/labels is best-effort: a failure logs and
// leaves that slice empty rather than sinking the whole run.
func (e *Extension) fetchSnapshot(ctx context.Context, owner, repo string, number int, isPR bool) (Snapshot, error) {
	var snap Snapshot
	snap.IsPR = isPR

	if isPR {
		m, err := e.app.pullMeta(ctx, owner, repo, number)
		if err != nil {
			return snap, fmt.Errorf("github: pullMeta: %w", err)
		}
		snap.Title, snap.Body, snap.State, snap.Labels = m.Title, m.Body, m.State, m.Labels
		snap.HeadRef, snap.HeadSHA, snap.BaseRef = m.HeadRef, m.HeadSHA, m.BaseRef
	} else {
		title, body, state, labels, err := e.app.issueMeta(ctx, owner, repo, number)
		if err != nil {
			return snap, fmt.Errorf("github: issueMeta: %w", err)
		}
		snap.Title, snap.Body, snap.State, snap.Labels = title, body, state, labels
	}

	if comments, err := e.app.listIssueComments(ctx, owner, repo, number); err != nil {
		slog.Warn("github: snapshot: listIssueComments failed", "component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Comments = make([]snapshotComment, 0, len(comments))
		for _, c := range comments {
			snap.Comments = append(snap.Comments, snapshotComment{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: c.User, UpdatedAt: c.UpdatedAt})
		}
	}

	if !isPR {
		return snap, nil
	}

	if d, err := e.app.listPRDiscussion(ctx, owner, repo, number); err != nil {
		slog.Warn("github: snapshot: listPRDiscussion failed", "component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		for _, r := range d.Reviews {
			snap.Reviews = append(snap.Reviews, snapshotReview{ID: r.ID, Body: r.Body, State: r.State, User: r.User, SubmittedAt: r.SubmittedAt})
		}
		for _, c := range d.ReviewComments {
			snap.ReviewComments = append(snap.ReviewComments, snapshotReviewComment{ID: c.ID, Path: c.Path, Line: c.Line, Body: c.Body, User: c.User, InReplyToID: c.InReplyToID})
		}
	}

	if files, err := e.app.pullFiles(ctx, owner, repo, number); err != nil {
		slog.Warn("github: snapshot: pullFiles failed", "component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Files = files
	}

	if commits, err := e.app.listPRCommits(ctx, owner, repo, number); err != nil {
		slog.Warn("github: snapshot: listPRCommits failed", "component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Commits = make([]snapshotCommit, 0, len(commits))
		for _, c := range commits {
			sc := snapshotCommit{SHA: c.SHA, Message: c.Message}
			diff, derr := e.app.commitDiff(ctx, owner, repo, c.SHA)
			if derr != nil {
				slog.Warn("github: snapshot: commitDiff failed; this commit's patch-id is unknown",
					"component", "github", "repo", owner+"/"+repo, "sha", c.SHA, "err", derr)
			} else if pid, perr := gitPatchID(ctx, diff); perr != nil {
				slog.Warn("github: snapshot: git patch-id failed", "component", "github", "sha", c.SHA, "err", perr)
			} else {
				sc.PatchID = pid
			}
			snap.Commits = append(snap.Commits, sc)
		}
	}

	return snap, nil
}

// gitPatchID computes a rebase-stable patch identity for one commit's unified
// diff, via `git patch-id --stable` reading the diff on stdin — no local
// clone or repository needed (patch-id parses the diff text itself), which is
// what lets a snapshot fetch happen at webhook time, before any node clones.
// "" (no error) for an empty diff (an empty commit, or a merge commit with no
// diffable content) — there's no patch identity to report.
func gitPatchID(ctx context.Context, diff string) (string, error) {
	if strings.TrimSpace(diff) == "" {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(diff)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// Delta is the semantic difference between two snapshots of the same
// issue/PR, keyed by stable identity (comment/review id, commit patch-id —
// never a SHA set-difference or a raw text diff; see diffSnapshots).
type Delta struct {
	TitleChanged        bool
	OldTitle, NewTitle  string
	BodyChanged         bool
	StateChanged        bool
	OldState, NewState  string
	LabelsAdded         []string
	LabelsRemoved       []string
	CommentsAdded       []snapshotComment
	CommentsEdited      []snapshotComment
	CommentsDeleted     []snapshotComment
	ReviewsAdded        []snapshotReview
	ReviewCommentsAdded []snapshotReviewComment
	// NewCommits are the commits in the new snapshot whose patch-id is not
	// present anywhere in the old snapshot's commits — genuinely new work,
	// robust across a rebase or force-push (see diffSnapshots). A commit
	// whose patch-id could not be computed (fetch/exec failure) is
	// conservatively treated as new: silently dropping it from review would
	// be the worse failure mode.
	NewCommits   []snapshotCommit
	FilesChanged bool
}

// Empty reports whether the delta carries nothing worth injecting — the
// resume case where GitHub looks exactly as it did last dispatch (#459's
// "resume with an unchanged snapshot injects an empty delta, not the whole
// thread again").
func (d Delta) Empty() bool {
	return !d.TitleChanged && !d.BodyChanged && !d.StateChanged &&
		len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 &&
		len(d.CommentsAdded) == 0 && len(d.CommentsEdited) == 0 && len(d.CommentsDeleted) == 0 &&
		len(d.ReviewsAdded) == 0 && len(d.ReviewCommentsAdded) == 0 &&
		len(d.NewCommits) == 0 && !d.FilesChanged
}

// diffSnapshots computes the semantic delta from old (the previously stored
// snapshot) to cur (freshly fetched) — the turn's context (#459 §2).
// excludeCommentID drops one comment id from the added/edited sets (the
// triggering comment itself, already quoted verbatim as "their request" —
// see excludeComment's old role); 0 excludes nothing.
func diffSnapshots(old, cur Snapshot, excludeCommentID int64) Delta {
	var d Delta

	if old.Title != cur.Title {
		d.TitleChanged, d.OldTitle, d.NewTitle = true, old.Title, cur.Title
	}
	if old.Body != cur.Body {
		d.BodyChanged = true
	}
	if old.State != cur.State {
		d.StateChanged, d.OldState, d.NewState = true, old.State, cur.State
	}
	oldLabels := map[string]bool{}
	for _, l := range old.Labels {
		oldLabels[l] = true
	}
	curLabels := map[string]bool{}
	for _, l := range cur.Labels {
		curLabels[l] = true
	}
	for _, l := range cur.Labels {
		if !oldLabels[l] {
			d.LabelsAdded = append(d.LabelsAdded, l)
		}
	}
	for _, l := range old.Labels {
		if !curLabels[l] {
			d.LabelsRemoved = append(d.LabelsRemoved, l)
		}
	}

	oldComments := make(map[int64]snapshotComment, len(old.Comments))
	for _, c := range old.Comments {
		oldComments[c.ID] = c
	}
	curIDs := make(map[int64]bool, len(cur.Comments))
	for _, c := range cur.Comments {
		curIDs[c.ID] = true
		if excludeCommentID != 0 && c.ID == excludeCommentID {
			continue
		}
		if prev, ok := oldComments[c.ID]; !ok {
			d.CommentsAdded = append(d.CommentsAdded, c)
		} else if prev.Body != c.Body {
			d.CommentsEdited = append(d.CommentsEdited, c)
		}
	}
	for _, c := range old.Comments {
		if !curIDs[c.ID] {
			d.CommentsDeleted = append(d.CommentsDeleted, c)
		}
	}

	oldReviewIDs := map[int64]bool{}
	for _, r := range old.Reviews {
		oldReviewIDs[r.ID] = true
	}
	for _, r := range cur.Reviews {
		if !oldReviewIDs[r.ID] {
			d.ReviewsAdded = append(d.ReviewsAdded, r)
		}
	}

	oldReviewCommentIDs := map[int64]bool{}
	for _, c := range old.ReviewComments {
		oldReviewCommentIDs[c.ID] = true
	}
	for _, c := range cur.ReviewComments {
		if !oldReviewCommentIDs[c.ID] {
			d.ReviewCommentsAdded = append(d.ReviewCommentsAdded, c)
		}
	}

	oldPatchIDs := map[string]bool{}
	for _, c := range old.Commits {
		if c.PatchID != "" {
			oldPatchIDs[c.PatchID] = true
		}
	}
	for _, c := range cur.Commits {
		if c.PatchID == "" || !oldPatchIDs[c.PatchID] {
			d.NewCommits = append(d.NewCommits, c)
		}
	}

	d.FilesChanged = len(old.Files) != len(cur.Files)

	return d
}

// renderDeltaDetail renders a delta as prose ready to inject as the turn's
// context — the "resume" half of #459 §3 ("resume injects only the delta").
// "" when the delta is empty.
func renderDeltaDetail(d Delta) string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Since your last look at this thread, the following changed on GitHub:\n")
	if d.TitleChanged {
		fmt.Fprintf(&b, "- Title changed: %q → %q\n", d.OldTitle, d.NewTitle)
	}
	if d.BodyChanged {
		b.WriteString("- The description was edited.\n")
	}
	if d.StateChanged {
		fmt.Fprintf(&b, "- State changed: %s → %s\n", d.OldState, d.NewState)
	}
	if len(d.LabelsAdded) > 0 {
		fmt.Fprintf(&b, "- Labels added: %s\n", strings.Join(d.LabelsAdded, ", "))
	}
	if len(d.LabelsRemoved) > 0 {
		fmt.Fprintf(&b, "- Labels removed: %s\n", strings.Join(d.LabelsRemoved, ", "))
	}
	for _, c := range d.CommentsAdded {
		fmt.Fprintf(&b, "- New comment from @%s: %s\n", c.User, truncate(c.Body, 2000))
	}
	for _, c := range d.CommentsEdited {
		fmt.Fprintf(&b, "- Edited comment from @%s (now reads): %s\n", c.User, truncate(c.Body, 2000))
	}
	for _, c := range d.CommentsDeleted {
		fmt.Fprintf(&b, "- Deleted comment from @%s (was): %s\n", c.User, truncate(c.Body, 300))
	}
	for _, r := range d.ReviewsAdded {
		fmt.Fprintf(&b, "- New review from @%s [%s]: %s\n", r.User, r.State, truncate(r.Body, 500))
	}
	for _, c := range d.ReviewCommentsAdded {
		fmt.Fprintf(&b, "- New inline comment from @%s on %s:%d: %s\n", c.User, c.Path, c.Line, truncate(c.Body, 500))
	}
	for _, c := range d.NewCommits {
		fmt.Fprintf(&b, "- New commit %s: %s\n", shortSHA(c.SHA), truncate(c.Message, 200))
	}
	return b.String()
}

// renderSeedContext renders a snapshot's full non-hidden context — the
// "first load seeds the session" half of #459 §3. excludeCommentID drops the
// triggering comment (already quoted separately as the request).
func renderSeedContext(snap Snapshot, excludeCommentID int64) string {
	var b strings.Builder
	if body := strings.TrimSpace(snap.Body); body != "" {
		fmt.Fprintf(&b, "Description:\n%s\n\n", truncate(body, 4000))
	}
	visible := make([]commentView, 0, len(snap.Comments))
	for _, c := range snap.Comments {
		if c.Hidden || (excludeCommentID != 0 && c.ID == excludeCommentID) {
			continue
		}
		visible = append(visible, commentView{ID: c.ID, Body: c.Body, User: c.User})
	}
	if len(visible) > 0 {
		const maxComments = 40
		b.WriteString("Comments so far (oldest first):\n")
		start := 0
		if len(visible) > maxComments {
			fmt.Fprintf(&b, "  … %d earlier comments omitted\n", len(visible)-maxComments)
			start = len(visible) - maxComments
		}
		for _, c := range visible[start:] {
			fmt.Fprintf(&b, "  @%s: %s\n", c.User, truncate(c.Body, 2000))
		}
	}
	if snap.IsPR {
		// Files are NOT rendered here — runMessage renders the CURRENT changed
		// files list itself, every dispatch (not just first load): it's
		// operational "what's in this diff" data for the reviewer, not part of
		// the conversation history this function seeds.
		disc := prDiscussion{}
		for _, r := range snap.Reviews {
			disc.Reviews = append(disc.Reviews, reviewView{Body: r.Body, State: r.State, User: r.User, SubmittedAt: r.SubmittedAt})
		}
		for _, c := range snap.ReviewComments {
			if c.Resolved {
				continue
			}
			disc.ReviewComments = append(disc.ReviewComments, reviewCommentView{Path: c.Path, Line: c.Line, Body: c.Body, User: c.User})
		}
		if s := discussionSummary(disc); s != "" {
			b.WriteString("\nExisting discussion — take it into account, do NOT repeat it:\n" + s)
		}
	}
	return b.String()
}

// shortSHA truncates a commit SHA to its conventional 7-char display form;
// anything shorter is returned as-is.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// marshalSnapshot/unmarshalSnapshot are the store's opaque JSON
// encode/decode for a Snapshot — split out so loadGithubContext reads as the
// fetch→diff→persist sequence the spec describes, not JSON plumbing.
func marshalSnapshot(s Snapshot) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalSnapshot(s string) (Snapshot, error) {
	var out Snapshot
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}
