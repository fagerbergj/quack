package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
//
// live (#605) is fork-replay's escape hatch: when the session hands back a
// *replay.ForkSignal instead of a recorded response, the call is answered by
// live instead - nil in strict mode (NewReplayModel), where a ForkSignal
// never occurs (Session.EnableFork was never called).
type replayModel struct {
	name    string
	session *replay.Session
	live    model.LLM
}

// NewReplayModel builds a replay-backed model.LLM over an already-loaded
// Session. NewModel's kind "replay" case uses this internally; a caller
// managing its OWN Session (replaytest, so one Session's divergence report
// covers every model AND tool a node uses) should call it directly instead
// of NewModel, to avoid loading the same bundle once per model.
func NewReplayModel(sess *replay.Session, name string) model.LLM {
	return &tracedModel{LLM: &replayModel{name: name, session: sess}, name: name}
}

// NewReplayModelFork is NewReplayModel with a live fallback: NewModel's kind
// "replay" + fork_mode: fork case uses this, live built from the provider's
// `live` config (the SAME factory, NewModel again - see factory.go). Every
// OTHER caller (replaytest, ACP playback, NewReplayModel itself) keeps
// getting live == nil, so a ForkSignal there surfaces as a plain error
// rather than silently reaching the network - fork-replay only activates
// where the caller deliberately opted in.
func NewReplayModelFork(sess *replay.Session, name string, live model.LLM) model.LLM {
	return &tracedModel{LLM: &replayModel{name: name, session: sess, live: live}, name: name}
}

func (m *replayModel) Name() string { return m.name }

func (m *replayModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var sysJSON []byte
		if req.Config != nil && req.Config.SystemInstruction != nil {
			// Same marshal shape emitChatEvent records gen_ai.system_instructions
			// from - the hash Session.NextChat compares must be computed over
			// identical bytes to mean anything.
			sysJSON, _ = json.Marshal(req.Config.SystemInstruction)
		}
		resp, err := m.session.NextChat(ledger.CoordsFromContext(ctx), m.name, sysJSON)
		var fs *replay.ForkSignal
		if errors.As(err, &fs) {
			if m.live == nil {
				yield(nil, fmt.Errorf("inference: replay: model %q forked to live but no live delegate is configured: %w", m.name, fs))
				return
			}
			for r, lerr := range m.live.GenerateContent(ctx, req, stream) {
				if !yield(r, lerr) {
					return
				}
			}
			return
		}
		yield(resp, err)
	}
}
