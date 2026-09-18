// judgereads.go: counts judge read-tool calls; a pass with zero reads is discarded.
// Only a PASS is checked (a fail without reads is conservative, not dangerous).
package vetting

import (
	"strings"
	"sync/atomic"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// readCounter: tally for one judge round's read-tool calls.
type readCounter struct {
	n        atomic.Int64
	hadTools bool // tool-less judge is never faulted
}

func (c *readCounter) count() int64 { return c.n.Load() }

// countingTool: wraps a read-only tool to tally invocations (mirrors guardedTool).
type countingTool struct {
	inner runnableTool
	c     *readCounter
}

// runnableTool: what functiontool-built tool.Tool satisfies (local to avoid vetting→tools import cycle).
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

func (t *countingTool) Name() string        { return t.inner.Name() }
func (t *countingTool) Description() string { return t.inner.Description() }
func (t *countingTool) IsLongRunning() bool { return t.inner.IsLongRunning() }
func (t *countingTool) Declaration() *genai.FunctionDeclaration {
	return t.inner.Declaration()
}

// ProcessRequest packs the wrapper, not the inner tool, into the request's
// tool map (mirrors guardedTool) - packing inner here would register the
// unwrapped tool for dispatch and this wrapper's Run would never be called.
func (t *countingTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, t)
}

func (t *countingTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	t.c.n.Add(1)
	return t.inner.Run(ctx, args)
}

// countReads: wraps tools to tally reads. Non-runnableTool passes through uncounted (more lenient, never harsher).
func countReads(tools []tool.Tool) ([]tool.Tool, *readCounter) {
	c := &readCounter{hadTools: len(tools) > 0}
	out := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if rt, ok := t.(runnableTool); ok {
			out = append(out, &countingTool{inner: rt, c: c})
			continue
		}
		out = append(out, t)
	}
	return out, c
}

// unreadPass: judge held read tools, passed, and never called one.
func unreadPass(c *readCounter, v verdict) bool {
	return c != nil && c.hadTools && v.Passed && c.count() == 0
}

// unreadPassFeedback: appended to judge prompt on re-run (states mechanism, not repeated instruction).
const unreadPassFeedback = "Your previous verdict passed this answer without opening a single file. " +
	"Read-tool calls are counted: a pass with zero reads is discarded, because nothing in it was verified " +
	"against the repository. Identify every specific claim the answer makes about this repo, check each one " +
	"with grep/glob/read_file, and score from what you actually found."

// judgeReadCounters: one round's repo-read and artifact-read tallies, kept
// separate so each zero-reads discard rule keys on its own tool set.
type judgeReadCounters struct {
	repo     *readCounter
	artifact *readCounter
}

// unreadArtifactPass: the worker wrote/edited an artifact this round, the
// judge passed, and never called read_artifact - nothing in the verdict
// checked what the worker actually produced.
func unreadArtifactPass(c *readCounter, v verdict, workerWroteArtifact bool) bool {
	return c != nil && workerWroteArtifact && v.Passed && c.count() == 0
}

// unreadArtifactPassFeedback: appended to the judge prompt on re-run, naming
// the exact ids so the retry has no excuse to skip them.
func unreadArtifactPassFeedback(ids []string) string {
	return "Your previous verdict passed this answer, but the worker wrote or edited an artifact this round " +
		"and you never called read_artifact. Read it before judging: " + strings.Join(ids, ", ") + ". " +
		"If the answer refers to an artifact instead of containing the deliverable, the artifact's content IS the answer."
}
