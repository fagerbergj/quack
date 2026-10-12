// Snapshot-and-diff session context: fetch full GitHub state, diff against the stored snapshot, inject the delta.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// snapshotComment: one comment in a Snapshot.
type snapshotComment struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Body      string `json:"body"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at,omitempty"`
	// Hidden: minimized comment. TODO: always false until GraphQL is wired.
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

// snapshotReviewComment: inline PR review comment.
type snapshotReviewComment struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Body        string `json:"body"`
	User        string `json:"user"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
	// Resolved: review thread isResolved. TODO: always false until GraphQL is wired.
	Resolved bool `json:"resolved,omitempty"`
}

// snapshotCommit: identified by rebase-stable patch-id (SHA changes on rebase).
type snapshotCommit struct {
	SHA     string `json:"sha"`
	PatchID string `json:"patch_id"`
	Message string `json:"message"`
}

// Snapshot: full GitHub state for issue/PR session. Cherry-picking at render time only.
type Snapshot struct {
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	State    string            `json:"state"`
	Merged   bool              `json:"merged,omitempty"`
	Draft    bool              `json:"draft,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Comments []snapshotComment `json:"comments,omitempty"`
	IsPR     bool              `json:"is_pr,omitempty"`
	HeadRef  string            `json:"head_ref,omitempty"`
	HeadSHA  string            `json:"head_sha,omitempty"`
	BaseRef  string            `json:"base_ref,omitempty"`
	// Fork: the PR's head repo differs from its base; computeGrant must never offer a push for one.
	Fork           bool                    `json:"fork,omitempty"`
	Reviews        []snapshotReview        `json:"reviews,omitempty"`
	ReviewComments []snapshotReviewComment `json:"review_comments,omitempty"`
	Commits        []snapshotCommit        `json:"commits,omitempty"`
	Files          []changedFile           `json:"files,omitempty"`
}

// fetchSnapshot fetches the current full GitHub state for one issue/PR. Every sub-fetch past the
// required meta call is best-effort: a failure logs and leaves that slice empty.
func (e *Extension) fetchSnapshot(ctx context.Context, owner, repo string, number int, isPR bool) (Snapshot, error) {
	var snap Snapshot
	snap.IsPR = isPR

	if isPR {
		m, err := e.app.pullMeta(ctx, owner, repo, number)
		if err != nil {
			return snap, fmt.Errorf("github: pullMeta: %w", err)
		}
		snap.Title, snap.Body, snap.State, snap.Draft, snap.Merged = m.Title, m.Body, m.State, m.Draft, m.Merged
		snap.Labels = m.Labels
		snap.HeadRef, snap.HeadSHA, snap.BaseRef = m.HeadRef, m.HeadSHA, m.BaseRef
		snap.Fork = m.Fork
	} else {
		title, body, state, labels, _, err := e.app.issueMeta(ctx, owner, repo, number)
		if err != nil {
			return snap, fmt.Errorf("github: issueMeta: %w", err)
		}
		snap.Title, snap.Body, snap.State, snap.Labels = title, body, state, labels
	}

	if comments, err := e.app.listIssueComments(ctx, owner, repo, number); err != nil {
		e.host.Log.Warn("github: snapshot: listIssueComments failed", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Comments = make([]snapshotComment, 0, len(comments))
		for _, c := range comments {
			snap.Comments = append(snap.Comments, snapshotComment{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: c.User, CreatedAt: c.CreatedAt})
		}
	}

	if !isPR {
		return snap, nil
	}

	if d, err := e.app.listPRDiscussion(ctx, owner, repo, number); err != nil {
		e.host.Log.Warn("github: snapshot: listPRDiscussion failed", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		for _, r := range d.Reviews {
			snap.Reviews = append(snap.Reviews, snapshotReview(r))
		}
		for _, c := range d.ReviewComments {
			snap.ReviewComments = append(snap.ReviewComments, snapshotReviewComment{ID: c.ID, Path: c.Path, Line: c.Line, Body: c.Body, User: c.User, InReplyToID: c.InReplyToID})
		}
	}

	if files, err := e.app.pullFiles(ctx, owner, repo, number); err != nil {
		e.host.Log.Warn("github: snapshot: pullFiles failed", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Files = files
	}

	if commits, err := e.app.listPRCommits(ctx, owner, repo, number); err != nil {
		e.host.Log.Warn("github: snapshot: listPRCommits failed", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Commits = make([]snapshotCommit, 0, len(commits))
		for _, c := range commits {
			sc := snapshotCommit{SHA: c.SHA, Message: c.Message}
			diff, derr := e.app.commitDiff(ctx, owner, repo, c.SHA)
			if derr != nil {
				e.host.Log.Warn("github: snapshot: commitDiff failed; this commit's patch-id is unknown",
					"repo", owner+"/"+repo, "sha", c.SHA, "err", derr)
			} else if pid, perr := gitPatchID(ctx, diff); perr != nil {
				e.host.Log.Warn("github: snapshot: git patch-id failed", "sha", c.SHA, "err", perr)
			} else {
				sc.PatchID = pid
			}
			snap.Commits = append(snap.Commits, sc)
		}
	}

	return snap, nil
}

// gitPatchID computes a rebase-stable patch identity via `git patch-id --stable` on stdin; no clone
// needed, so it runs at webhook time. "" (no error) for an empty diff.
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

// Delta is the semantic difference between two snapshots, keyed by stable identity
// (comment/review id, commit patch-id), never a SHA set-difference or raw text diff.
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
	// NewCommits: patch-id absent from the old snapshot, so a rebase isn't new work. An uncomputable
	// patch-id counts as new: silently dropping it from review is the worse failure.
	NewCommits   []snapshotCommit
	FilesChanged bool
}

// Empty reports whether the delta carries nothing worth injecting.
func (d Delta) Empty() bool {
	return !d.TitleChanged && !d.BodyChanged && !d.StateChanged &&
		len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 &&
		len(d.CommentsAdded) == 0 && len(d.CommentsEdited) == 0 && len(d.CommentsDeleted) == 0 &&
		len(d.ReviewsAdded) == 0 && len(d.ReviewCommentsAdded) == 0 &&
		len(d.NewCommits) == 0 && !d.FilesChanged
}

// diffSnapshots computes the delta from the stored snapshot to the fresh one. excludeCommentID drops the
// triggering comment (already quoted as "their request") from added/edited; 0 excludes nothing.
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
	d.LabelsAdded, d.LabelsRemoved = labelDelta(old.Labels, cur.Labels)
	d.CommentsAdded, d.CommentsEdited, d.CommentsDeleted = commentDelta(old.Comments, cur.Comments, excludeCommentID)
	d.ReviewsAdded = addedSince(old.Reviews, cur.Reviews, func(r snapshotReview) int64 { return r.ID })
	d.ReviewCommentsAdded = addedSince(old.ReviewComments, cur.ReviewComments, func(c snapshotReviewComment) int64 { return c.ID })
	d.NewCommits = newCommitsSince(old.Commits, cur.Commits)

	d.FilesChanged = len(old.Files) != len(cur.Files)

	return d
}

// labelDelta: the labels present only in cur / only in old, in their list order.
func labelDelta(oldLabels, curLabels []string) (added, removed []string) {
	for _, l := range curLabels {
		if !slices.Contains(oldLabels, l) {
			added = append(added, l)
		}
	}
	for _, l := range oldLabels {
		if !slices.Contains(curLabels, l) {
			removed = append(removed, l)
		}
	}
	return added, removed
}

// commentDelta: comments added/edited/deleted between two snapshots; excludeCommentID drops the
// triggering comment from added/edited, never from deletion detection.
func commentDelta(oldComments, curComments []snapshotComment, excludeCommentID int64) (added, edited, deleted []snapshotComment) {
	oldCommentsMap := make(map[int64]snapshotComment, len(oldComments))
	for _, c := range oldComments {
		oldCommentsMap[c.ID] = c
	}
	curIDs := make(map[int64]bool, len(curComments))
	for _, c := range curComments {
		curIDs[c.ID] = true
		if excludeCommentID != 0 && c.ID == excludeCommentID {
			continue
		}
		if prev, ok := oldCommentsMap[c.ID]; !ok {
			added = append(added, c)
		} else if prev.Body != c.Body {
			edited = append(edited, c)
		}
	}
	for _, c := range oldComments {
		if !curIDs[c.ID] {
			deleted = append(deleted, c)
		}
	}
	return added, edited, deleted
}

// addedSince: the items in cur whose stable ID is absent from old -
// reviews and review comments key on their numeric ID.
func addedSince[T any](oldItems, curItems []T, id func(T) int64) []T {
	oldIDs := map[int64]bool{}
	for _, item := range oldItems {
		oldIDs[id(item)] = true
	}
	var out []T
	for _, item := range curItems {
		if !oldIDs[id(item)] {
			out = append(out, item)
		}
	}
	return out
}

// newCommitsSince: cur's commits with a patch-id absent from old.
func newCommitsSince(oldCommits, curCommits []snapshotCommit) []snapshotCommit {
	oldPatchIDs := map[string]bool{}
	for _, c := range oldCommits {
		oldPatchIDs[c.PatchID] = true
	}
	return newCommitsAgainstBaseline(curCommits, oldPatchIDs)
}

// shortSHA truncates a commit SHA to its 7-char display form.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// newCommitsAgainstBaseline returns commits whose patch-id is not in reviewed; an uncomputable
// patch-id counts as new.
func newCommitsAgainstBaseline(commits []snapshotCommit, reviewed map[string]bool) []snapshotCommit {
	out := make([]snapshotCommit, 0, len(commits)) // non-nil even when empty: nil means "no baseline at all" (see reviewScope)
	for _, c := range commits {
		if c.PatchID == "" || !reviewed[c.PatchID] {
			out = append(out, c)
		}
	}
	return out
}

// marshalJSON/unmarshalJSON are the store's opaque encode/decode for snapshots and review baselines.
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func unmarshalJSON[T any](s string) (T, error) {
	var out T
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}
