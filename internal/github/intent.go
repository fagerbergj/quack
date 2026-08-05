package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
)

// IntentClassifier is a single model round-trip: prompt in, raw text out.
// Defined here (not a concrete model type) so tests can fake it without a
// real model, and this package stays free of any inference/ADK dependency.
type IntentClassifier interface {
	Classify(ctx context.Context, prompt string) (string, error)
}

// intentClassifierTimeout bounds the classification call - it runs inline in
// webhook dispatch, so a slow/hung model must not stall the run.
const intentClassifierTimeout = 5 * time.Second

// intentClassifierPrompt calls out the exact failure modes that tripped up
// the regex this classifier replaces: quoted code, declines, corrections.
const intentClassifierPrompt = `You classify a single GitHub comment as WORK or CONVERSATIONAL.

WORK means the user is asking for review or implementation work to be done now - e.g. "review this PR", "focus on the auth path", "please fix the lint errors", "implement this and push a branch".

CONVERSATIONAL means anything else - a question, a clarification, a correction, an opinion, or small talk. In particular, treat these as CONVERSATIONAL:
- Code quoted or referenced in the message (e.g. "it.migrate(connection) throws here") - a method call inside a code snippet is not an instruction to the reader.
- A message declining or deferring work ("no need to re-review that", "don't bother re-running it").
- A correction to something already said ("that finding was wrong", "you misread the diff") - this is feedback about the past, not a new request.

Reply with exactly one word: WORK or CONVERSATIONAL. No punctuation, no explanation.

Message:
%s`

// isWorkRequest classifies a PR mention as work (review framing) or
// conversational (answer-from-context framing). Fails safe: nil classifier,
// error, timeout, or unparseable answer all resolve to conversational.
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
	// Substring, not equality: a small instruct model wraps its one word often
	// enough ("**WORK**", "WORK.") that an exact match would silently answer
	// conversational for every genuine review request. CONVERSATIONAL is tested
	// first so the longer word can't be shadowed by the "WORK" inside it.
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

// deliverablePrompt asks a bounded question: given the message, is the asker
// after a review or a code change? Only reachable when the grant permits
// BOTH (#689) - the prompt never offers a choice the grant would refuse, so
// an answer this classifier accepts is safe by construction.
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
