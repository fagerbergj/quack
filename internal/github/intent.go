package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
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
	switch strings.ToUpper(strings.TrimSpace(answer)) {
	case "WORK":
		return true
	case "CONVERSATIONAL":
		return false
	default:
		slog.Warn("github: intent classifier returned an unparseable answer; treating mention as conversational", "component", "github", "answer", answer)
		return false
	}
}
