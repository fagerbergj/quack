package tools

import (
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
type replayToolStub struct {
	name    string
	session *replay.Session
	coords  ledger.Coords
}

// newReplayStubs builds one stub per requested name - no registry lookup,
// no Deps beyond the session, so an agent's tools: list resolves identically
// whether it names a builtin or an extension tool; replay doesn't care which.
// coords (Deps.LedgerCoords) is stamped the same way emitTool's is - a tool
// call's ctx doesn't reliably carry it (see emitTool's doc comment); zero
// value falls back to ctx.
func newReplayStubs(names []string, sess *replay.Session, coords ledger.Coords) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, &replayToolStub{name: name, session: sess, coords: coords})
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
	return r.session.NextToolResult(coords, r.name, args)
}
