package inference

import (
	"context"
	"encoding/json"
	"iter"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
)

// replayModel answers GenerateContent from a loaded replay.Session instead
// of a live endpoint - kind "replay" (NewModel). It resolves its stream from
// the SAME ledger.Coords a live worker/judge round stamps onto ctx
// (runWorkerNodeTraced / the gate's judge round), so no call site anywhere
// else needs to know replay is active. A structural miss (replay.MissError)
// is yielded as the call's own error - replay-strict never falls back to a
// live call (.quack/replay-log.md).
type replayModel struct {
	name    string
	session *replay.Session
}

// NewReplayModel builds a replay-backed model.LLM over an already-loaded
// Session. NewModel's kind "replay" case uses this internally; a caller
// managing its OWN Session (replaytest, so one Session's divergence report
// covers every model AND tool a node uses) should call it directly instead
// of NewModel, to avoid loading the same bundle once per model.
func NewReplayModel(sess *replay.Session, name string) model.LLM {
	return &tracedModel{LLM: &replayModel{name: name, session: sess}, name: name}
}

func (m *replayModel) Name() string { return m.name }

func (m *replayModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var sysJSON []byte
		if req.Config != nil && req.Config.SystemInstruction != nil {
			// Same marshal shape emitChatEvent records gen_ai.system_instructions
			// from - the hash Session.NextChat compares must be computed over
			// identical bytes to mean anything.
			sysJSON, _ = json.Marshal(req.Config.SystemInstruction)
		}
		resp, err := m.session.NextChat(ledger.CoordsFromContext(ctx), m.name, sysJSON)
		yield(resp, err)
	}
}
