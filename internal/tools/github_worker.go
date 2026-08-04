package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
)

// This file threads the two per-node evidence scopings a GitHub trigger
// computes (#664, consumer split) from the webhook dispatch boundary to
// tools.NewPlanTool, the SAME way WithGitHubSetup/WithGrant already do:
// values the model must never author itself, read exactly once at the top of
// Orchestrator.Run.

type workerAskContextKey struct{}

// WithWorkerAsk attaches the ask-only text (permissions, deliverable,
// title/body/comments - never evidence) a GitHub-triggered plan's nodes get
// as their BACKGROUND, in place of the orchestrator's own full envelope. Call
// ONLY from the GitHub webhook dispatch boundary.
func WithWorkerAsk(ctx context.Context, ask string) context.Context {
	return context.WithValue(ctx, workerAskContextKey{}, ask)
}

// WorkerAskFromContext reads back the ask WithWorkerAsk attached, if any. ""
// means no GitHub trigger governs this run - dag.Plan.WorkerBackground stays
// unset and buildTask falls back to UserMessage, unchanged from before #664.
func WorkerAskFromContext(ctx context.Context) string {
	s, _ := ctx.Value(workerAskContextKey{}).(string)
	return s
}

type ciChecksContextKey struct{}

// WithCIChecks attaches a CI-fix run's failing checks, each with its own
// rendered annotation detail - computed once at dispatch (cifix.go's
// failingChecks), never re-derived from model output. buildTask hands a
// check's detail only to the node whose own task names it (dag.CICheck).
func WithCIChecks(ctx context.Context, checks []dag.CICheck) context.Context {
	return context.WithValue(ctx, ciChecksContextKey{}, checks)
}

// CIChecksFromContext reads back the checks WithCIChecks attached, if any.
// nil for anything but a CI-triggered run.
func CIChecksFromContext(ctx context.Context) []dag.CICheck {
	c, _ := ctx.Value(ciChecksContextKey{}).([]dag.CICheck)
	return c
}
