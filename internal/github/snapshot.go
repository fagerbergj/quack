// Snapshot-and-diff session context: fetches full GitHub state, diffs against stored snapshot, injects delta only.
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

// snapshotComment: one comment in a Snapshot.
type snapshotComment struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Body      string `json:"body"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Hidden: minimized comment (TODO: always false, needs GraphQL). Left as a seam.
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
	// Resolved: review thread isResolved (GraphQL). TODO: always false; needs GraphQL wiring.
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
	// Fork is true when this PR's head repo differs from its base repo -
	// carried through to computeGrant, which must never offer
	// push_commits_to_pr for one (#662). Always false for an issue (IsPR
	// false).
	Fork           bool                    `json:"fork,omitempty"`
	Reviews        []snapshotReview        `json:"reviews,omitempty"`
	ReviewComments []snapshotReviewComment `json:"review_comments,omitempty"`
	Commits        []snapshotCommit        `json:"commits,omitempty"`
	Files          []changedFile           `json:"files,omitempty"`
}

// fetchSnapshot fetches the CURRENT full GitHub state for one issue/PR - the
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
		slog.Warn("github: snapshot: listIssueComments failed", "component", "github", "repo", owner+"/"+repo, "number", number, "err", err)
	} else {
		snap.Comments = make([]snapshotComment, 0, len(comments))
		for _, c := range comments {
			snap.Comments = append(snap.Comments, snapshotComment{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: c.User, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
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
// diff, via `git patch-id --stable` reading the diff on stdin - no local
// clone or repository needed (patch-id parses the diff text itself), which is
// what lets a snapshot fetch happen at webhook time, before any node clones.
// "" (no error) for an empty diff (an empty commit, or a merge commit with no
// diffable content) - there's no patch identity to report.
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
// issue/PR, keyed by stable identity (comment/review id, commit patch-id -
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
	// present anywhere in the old snapshot's commits - genuinely new work,
	// robust across a rebase or force-push (see diffSnapshots). A commit
	// whose patch-id could not be computed (fetch/exec failure) is
	// conservatively treated as new: silently dropping it from review would
	// be the worse failure mode.
	NewCommits   []snapshotCommit
	FilesChanged bool
}

// Empty reports whether the delta carries nothing worth injecting - the
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
// snapshot) to cur (freshly fetched) - the turn's context (#459 §2).
// excludeCommentID drops one comment id from the added/edited sets (the
// triggering comment itself, already quoted verbatim as "their request" -
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

// shortSHA truncates a commit SHA to its conventional 7-char display form;
// anything shorter is returned as-is.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// newCommitsAgainstBaseline returns the commits whose patch-id is NOT in
// `reviewed` - the review scope. Deliberately decoupled from diffSnapshots'
// NewCommits, which advances on EVERY dispatch and would under-scope a review
// whenever a conversational dispatch landed in between. `reviewed` is the
// patch-id set from the last DELIVERED review only; an uncomputable patch-id
// is conservatively treated as new.
func newCommitsAgainstBaseline(commits []snapshotCommit, reviewed map[string]bool) []snapshotCommit {
	out := make([]snapshotCommit, 0, len(commits)) // non-nil even when empty: nil means "no baseline at all" (see reviewScope)
	for _, c := range commits {
		if c.PatchID == "" || !reviewed[c.PatchID] {
			out = append(out, c)
		}
	}
	return out
}

// marshalPatchIDs/unmarshalPatchIDs are the review baseline's opaque JSON
// encode/decode (a []string of patch-ids), mirroring marshalSnapshot below.
func marshalPatchIDs(ids []string) (string, error) {
	b, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalPatchIDs(s string) ([]string, error) {
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// marshalSnapshot/unmarshalSnapshot are the store's opaque JSON
// encode/decode for a Snapshot - split out so loadGithubContext reads as the
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
