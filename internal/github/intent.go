package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
)

// IntentClassifier: single model round-trip (interface so tests can fake it).
type IntentClassifier interface {
	Classify(ctx context.Context, prompt string) (string, error)
}

// Bounds classification (runs inline in webhook dispatch).
const intentClassifierTimeout = 5 * time.Second

// intentClassifierPrompt: replaces regex classifier; handles quoted code, declines, corrections.
const intentClassifierPrompt = `You classify a single GitHub comment as WORK or CONVERSATIONAL.

WORK means the user is asking for review or implementation work to be done now - e.g. "review this PR", "focus on the auth path", "please fix the lint errors", "implement this and push a branch".

CONVERSATIONAL means anything else - a question, a clarification, a correction, an opinion, or small talk. In particular, treat these as CONVERSATIONAL:
- Code quoted or referenced in the message (e.g. "it.migrate(connection) throws here") - a method call inside a code snippet is not an instruction to the reader.
- A message declining or deferring work ("no need to re-review that", "don't bother re-running it").
- A correction to something already said ("that finding was wrong", "you misread the diff") - this is feedback about the past, not a new request.

Reply with exactly one word: WORK or CONVERSATIONAL. No punctuation, no explanation.

Message:
%s`

// isWorkRequest: PR mention → work or conversational. Fails safe to conversational.
func (e *Extension) isWorkRequest(ctx context.Context, task string) bool {
	if e.intentClassifier == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, intentClassifierTimeout)
	defer cancel()
	answer, err := e.intentClassifier.Classify(ctx, fmt.Sprintf(intentClassifierPrompt, task))
	if err != nil {
		slog.Warn("github: intent classifier failed; treating mention as conversational", "component", "github", "err", err)
		return false
	}
	// Substring match (small models often wrap output in ** or punctuation).
	switch up := strings.ToUpper(strings.TrimSpace(answer)); {
	case strings.Contains(up, "CONVERSATIONAL"):
		return false
	case strings.Contains(up, "WORK"):
		return true
	default:
		slog.Warn("github: intent classifier returned an unparseable answer; treating mention as conversational", "component", "github", "answer", answer)
		return false
	}
}

// deliverablePrompt: review vs commit? Only reachable when grant permits both (#689).
const deliverablePrompt = `You classify a single GitHub PR comment as REVIEW or COMMIT.

REVIEW means the asker wants the code assessed - e.g. "review this", "take another look", "double check the auth path".

COMMIT means the asker wants a code change made and delivered - e.g. "fix this", "address these findings", "make sure that's valid, then fix it", "implement what we discussed". A request to verify findings before acting is still COMMIT if it ends in an ask to act on them.

Reply with exactly one word: REVIEW or COMMIT. No punctuation, no explanation.

Message:
%s`

// classifyPRDeliverable picks which of the grant's permitted PR deliverables
// (review, commit) a message is asking for, in place of vetting.
// ImplementationIntent's regex guess. ok=false falls back to that regex
// (grant permits neither, classifier failed, or unparseable answer).
// When the grant permits only ONE, that's the answer by construction - no
// model call needed, but logged anyway (Debug) since the grant decided.
func (e *Extension) classifyPRDeliverable(ctx context.Context, task string, grant vetting.Grant) (kind string, ok bool) {
	canReview, canCommit := grant.PostReview, grant.PushCommitsToPR
	switch {
	case canReview && !canCommit:
		slog.Debug("github: PR deliverable bounded to the sole granted option", "component", "github", "kind", "review")
		return "review", true
	case canCommit && !canReview:
		slog.Debug("github: PR deliverable bounded to the sole granted option", "component", "github", "kind", "commit")
		return "commit", true
	case !canReview && !canCommit:
		return "", false
	}

	if e.intentClassifier == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, intentClassifierTimeout)
	defer cancel()
	answer, err := e.intentClassifier.Classify(ctx, fmt.Sprintf(deliverablePrompt, task))
	if err != nil {
		slog.Warn("github: deliverable classifier failed; falling back to the implement/review wording heuristic", "component", "github", "err", err)
		return "", false
	}
	switch up := strings.ToUpper(strings.TrimSpace(answer)); {
	case strings.Contains(up, "COMMIT"):
		return "commit", true
	case strings.Contains(up, "REVIEW"):
		return "review", true
	default:
		slog.Warn("github: deliverable classifier returned an unparseable answer; falling back to the implement/review wording heuristic", "component", "github", "answer", answer)
		return "", false
	}
}

// implementPrompt: implement vs comment? Only reachable when the grant permits open_pr (#713).
const implementPrompt = `You classify a single GitHub issue comment as IMPLEMENT or COMMENT.

IMPLEMENT means the asker wants code written and a pull request opened now - e.g. "implement this", "go ahead and build it", "the deliverable is a pull request, not a plan", "commit and stage the PR".

COMMENT means the asker wants a reply, discussion, or a plan - e.g. "what do you think", "can you clarify this", "draft a plan first", or a correction about a prior run.

Reply with exactly one word: IMPLEMENT or COMMENT. No punctuation, no explanation.

Message:
%s`

// classifyIssueDeliverable picks implement-vs-comment for an issue comment,
// mirroring classifyPRDeliverable (#691) on the issue side (#713): a comment
// is always a legal answer, but "implement" is legal only when the grant
// carries open_pr (quack:implement) - the classifier picks within that bound.
// ok=false falls back to vetting.ImplementationIntent's wording heuristic,
// never straight to conversational (a cold/erroring classifier must not
// silently invert an implementation request).
func (e *Extension) classifyIssueDeliverable(ctx context.Context, task string, grant vetting.Grant) (kind string, ok bool) {
	if !grant.OpenPR || e.intentClassifier == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, intentClassifierTimeout)
	defer cancel()
	answer, err := e.intentClassifier.Classify(ctx, fmt.Sprintf(implementPrompt, task))
	if err != nil {
		slog.Warn("github: issue deliverable classifier failed; falling back to the implement wording heuristic", "component", "github", "err", err)
		return "", false
	}
	switch up := strings.ToUpper(strings.TrimSpace(answer)); {
	case strings.Contains(up, "IMPLEMENT"):
		return "implement", true
	case strings.Contains(up, "COMMENT"):
		return "comment", true
	default:
		slog.Warn("github: issue deliverable classifier returned an unparseable answer; falling back to the implement wording heuristic", "component", "github", "answer", answer)
		return "", false
	}
}
