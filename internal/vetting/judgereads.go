// A judge that PASSES an answer without opening a single file has verified
// nothing - it rationalized. That is not hypothetical: #359 was a judge scoring
// 100% on a code-exploration answer by reasoning "the ledger shows they read
// exa.go" when the ledger was empty and the worker had web_fetched the file
// instead. The prompt has said verification is MANDATORY ever since; a prompt is
// not a mechanism, so this counts the reads and disbelieves the verdict.
//
// Only a PASS is checked. A judge that FAILS an answer without reading is being
// conservative, not dangerous, and re-running it would just cost a round.
package vetting

import (
	"sync/atomic"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// readCounter is the shared tally for one judge round: every wrapped read tool
// increments it, and the round's caller reads it after submit_verdict lands.
type readCounter struct {
	n atomic.Int64
	// hadTools records whether the judge was given any read tools at all - a
	// tool-less judge (pure-research deploys) can never be faulted for not
	// reading, and only the factory knows.
	hadTools bool
}

func (c *readCounter) count() int64 { return c.n.Load() }

// countingTool wraps one of the judge's read-only tools to tally invocations.
// It stands in for inner transparently - same name, same declaration - so the
// model cannot tell the difference (mirrors internal/tools' guardedTool).
type countingTool struct {
	inner runnableTool
	c     *readCounter
}

// runnableTool is what a functiontool-built tool.Tool actually satisfies. Kept
// local rather than exported from internal/tools: one interface declaration is
// cheaper than a dependency between the two packages.
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
func (t *countingTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return t.inner.ProcessRequest(ctx, req)
}

func (t *countingTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	t.c.n.Add(1)
	return t.inner.Run(ctx, args)
}

// countReads wraps every tool it can and returns the tally. A tool that is not a
// runnableTool passes through uncounted rather than failing the round: an
// uncounted read only ever makes the check MORE lenient, never wrongly harsh.
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

// unreadPass reports whether a verdict must not be trusted: the judge held read
// tools, passed the answer, and never called one.
func unreadPass(c *readCounter, v verdict) bool {
	return c != nil && c.hadTools && v.Passed && c.count() == 0
}

// unreadPassFeedback is appended to the judge's own prompt on the re-run. It
// states the mechanism rather than repeating the instruction it already ignored.
const unreadPassFeedback = "Your previous verdict passed this answer without opening a single file. " +
	"Read-tool calls are counted: a pass with zero reads is discarded, because nothing in it was verified " +
	"against the repository. Identify every specific claim the answer makes about this repo, check each one " +
	"with grep/glob/read_file, and score from what you actually found."
