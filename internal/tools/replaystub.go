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

// replayToolStub answers a tool call from a replay.Session instead of a real backend.
// live is fork-replay's escape hatch (nil in strict mode).
type replayToolStub struct {
	name    string
	session *replay.Session
	coords  ledger.Coords
	live    runnableTool // guard.go's structural interface: Declaration + Run
}

// newReplayStubs builds one stub per requested name from the recording session.
func newReplayStubs(names []string, sess *replay.Session, coords ledger.Coords) []tool.Tool {
	return newReplayStubsWithLive(names, sess, coords, nil)
}

// newReplayStubsWithLive: newReplayStubs with a live fallback per stub (fork mode).
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

// Declaration: shallow identity by name only - the recorded result returns regardless.
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
