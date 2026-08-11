package inference

import (
	"context"
	"fmt"
	"iter"
	"log/slog"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
)

// hydratingModel swaps artifactref reference parts for real bytes just
// before the model sees them. tracedModel wraps THIS (factory.go), so its
// gen_ai ledger logs the unmodified, reference-only request.
type hydratingModel struct {
	model.LLM
	artifacts artifact.Service
}

// HydratingModelForTesting wraps m like NewModel's "openai" branch does, for
// tests that need hydration without the full factory (config.ProviderConfig
// has no slot for a fake model.LLM).
func HydratingModelForTesting(m model.LLM, artifacts artifact.Service) model.LLM {
	return &hydratingModel{LLM: m, artifacts: artifacts}
}

// GenerateContent must never mutate req in place: tracedModel (the caller,
// factory.go) holds the SAME req pointer and logs it via emitChatEvent AFTER
// this returns - an in-place hydrate would leak real bytes into that ledger
// entry. hydrateRequest returns a distinct copy instead.
func (h *hydratingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	sendReq := req
	if h.artifacts != nil {
		if hydrated, changed := hydrateRequest(ctx, h.artifacts, req); changed {
			sendReq = hydrated
		}
	}
	return h.LLM.GenerateContent(ctx, sendReq, stream)
}

// Embed passes through - embedding requests carry no attachment parts.
func (h *hydratingModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e, ok := h.LLM.(Embedder)
	if !ok {
		return nil, fmt.Errorf("inference: model does not implement Embed")
	}
	return e.Embed(ctx, texts)
}

// hydrateRequest builds a shallow copy of req with reference parts replaced
// by real bytes, touching neither req nor its Contents slice - see the
// GenerateContent doc comment for why that matters.
func hydrateRequest(ctx context.Context, svc artifact.Service, req *model.LLMRequest) (*model.LLMRequest, bool) {
	newContents := make([]*genai.Content, len(req.Contents))
	anyChanged := false
	for i, c := range req.Contents {
		newContents[i] = c
		if c == nil {
			continue
		}
		newParts, changed := hydrateParts(ctx, svc, c.Parts)
		if changed {
			newContents[i] = &genai.Content{Role: c.Role, Parts: newParts}
			anyChanged = true
		}
	}
	if !anyChanged {
		return req, false
	}
	cp := *req
	cp.Contents = newContents
	return &cp, true
}

func hydrateParts(ctx context.Context, svc artifact.Service, parts []*genai.Part) ([]*genai.Part, bool) {
	out := make([]*genai.Part, 0, len(parts))
	changed := false
	for _, p := range parts {
		userID, sessionID, name, revision, ok := artifactref.Decode(p)
		if !ok {
			out = append(out, p)
			continue
		}
		resp, err := svc.Load(ctx, &artifact.LoadRequest{
			AppName: artifactref.AppName, UserID: userID, SessionID: sessionID, FileName: name, Version: revision,
		})
		if err != nil {
			slog.Warn("artifact hydration failed; the model will not see this attachment",
				"component", "inference", "name", name, "revision", revision, "err", err)
			out = append(out, p) // leave the reference in place rather than silently drop it
			continue
		}
		out = append(out, resp.Part)
		changed = true
	}
	return out, changed
}
