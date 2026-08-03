package tools

import (
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
)

// replayToolStub answers a tool call from a replay.Session instead of any
// real backend - kind:replay's tool seam (Deps.Replayer). Build returns one
// of these for every requested name instead of resolving the registry
// constructor, so a replay run never constructs a real backend (no
// searxng/crawl4ai/git dependency by construction) and never needs a clone
// or toolchain for the gate's probes either - both simply have nothing to
// reach. Guard/repeat/cancel wrapping is skipped for stubs (nothing live to
// guard against or cancel).
//
// live (#605) is fork-replay's escape hatch, mirroring inference.
// replayModel.live: nil in strict mode, where session.NextToolResult never
// yields a *replay.ForkSignal (EnableFork was never called).
type replayToolStub struct {
	name    string
	session *replay.Session
	coords  ledger.Coords
	live    runnableTool // guard.go's structural interface: Declaration + Run
}

// newReplayStubs builds one stub per requested name - no registry lookup,
// no Deps beyond the session, so an agent's tools: list resolves identically
// whether it names a builtin or an extension tool; replay doesn't care which.
// coords (Deps.LedgerCoords) is stamped the same way emitTool's is - a tool
// call's ctx doesn't reliably carry it (see emitTool's doc comment); zero
// value falls back to ctx.
func newReplayStubs(names []string, sess *replay.Session, coords ledger.Coords) []tool.Tool {
	return newReplayStubsWithLive(names, sess, coords, nil)
}

// newReplayStubsWithLive is newReplayStubs with a live fallback per stub,
// positionally zipped with names (live[i] backs names[i]; live may be
// shorter or nil - a name with no corresponding live tool just gets nil,
// same as newReplayStubs). Build's fork-mode branch (registry.go) is the one
// caller that supplies live, built by recursively calling Build with
// Replayer cleared - every entry Build returns satisfies runnableTool (it's
// what guardedTool/cancelGuard/pathScrub already require to wrap a tool
// transparently), so the assertion below can't fail in practice.
func newReplayStubsWithLive(names []string, sess *replay.Session, coords ledger.Coords, live []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for i, name := range names {
		var l runnableTool
		if i < len(live) {
			l, _ = live[i].(runnableTool)
		}
		out = append(out, &replayToolStub{name: name, session: sess, coords: coords, live: l})
	}
	return out
}

func (r *replayToolStub) Name() string        { return r.name }
func (r *replayToolStub) Description() string { return "replayed tool call, answered from a recording" }
func (r *replayToolStub) IsLongRunning() bool { return false }

// Declaration is deliberately permissive: matching (.quack/replay-log.md) is
// shallow identity by NAME only, so the schema only needs to let a live
// call through the ADK request path, never to shape or validate its args -
// the recorded result is what's returned regardless of what was asked.
func (r *replayToolStub) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:                 r.name,
		Description:          r.Description(),
		ParametersJsonSchema: map[string]any{"type": "object"},
	}
}

func (r *replayToolStub) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, r)
}

func (r *replayToolStub) Run(ctx agent.Context, args any) (map[string]any, error) {
	coords := r.coords
	if coords == (ledger.Coords{}) {
		coords = ledger.CoordsFromContext(ctx)
	}
	res, err := r.session.NextToolResult(coords, r.name, args)
	var fs *replay.ForkSignal
	if errors.As(err, &fs) {
		if r.live == nil {
			return nil, fmt.Errorf("tools: replay: %q forked to live but no live delegate is configured: %w", r.name, fs)
		}
		return r.live.Run(ctx, args)
	}
	return res, err
}
