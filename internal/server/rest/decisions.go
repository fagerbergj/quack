package rest

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/schema"
)

const (
	defaultDecisionLimit = 5000
	maxDecisionLimit     = 50000 // matches openapi.yaml
)

// ListDecisions serves every decision ledger entry across chats (backs `quack decisions`).
// The cross-chat read is one query; chat and point filters apply after it.
func (h *Handler) ListDecisions(w http.ResponseWriter, r *http.Request, params schema.ListDecisionsParams) {
	if h.ledgerStore == nil {
		errMsg(w, http.StatusNotFound, "recording is not enabled")
		return
	}
	limit := defaultDecisionLimit
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 1 || limit > maxDecisionLimit {
		errMsg(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxDecisionLimit))
		return
	}
	var since time.Time
	if params.Since != nil {
		since = *params.Since
	}
	entries, err := ledger.ReadAllByKindsSince(r.Context(), h.ledgerStore, []string{ledger.KindDecision}, since)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := schema.DecisionList{QuackVersion: h.quackVersion}
	data, skipped := decodeDecisions(entries, params)
	out.Data = data
	slices.SortStableFunc(out.Data, func(a, b schema.DecisionRecord) int { return a.At.Compare(b.At) })
	if skipped > 0 {
		out.Skipped = &skipped
	}
	if len(out.Data) > limit {
		trunc := true
		out.Data, out.Truncated = out.Data[len(out.Data)-limit:], &trunc
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeDecisions applies the chat and point filters and counts undecodable payloads.
func decodeDecisions(entries []ledger.Entry, params schema.ListDecisionsParams) ([]schema.DecisionRecord, int) {
	withState := params.WithState != nil && *params.WithState
	data, skipped := []schema.DecisionRecord{}, 0
	for _, e := range entries {
		if params.Chat != nil && !slices.Contains(*params.Chat, e.ChatID) {
			continue
		}
		var p ledger.DecisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			slog.Debug("decisions: skipping undecodable payload", "chat", e.ChatID, "seq", e.Seq, "err", err)
			skipped++
			continue
		}
		if params.Point != nil && p.Point != *params.Point {
			continue
		}
		data = append(data, decisionRecord(e, p, withState))
	}
	return data, skipped
}

func decisionRecord(e ledger.Entry, p ledger.DecisionPayload, withState bool) schema.DecisionRecord {
	rec := schema.DecisionRecord{
		ChatId: e.ChatID, NodeId: strPtr(e.NodeID), Round: strPtr(e.Round), At: e.At,
		Point: p.Point, Mode: p.Mode, Handler: p.Handler, Outcome: p.Outcome, Confident: p.Confident,
		Top: strPtr(p.Top), TopP: &p.TopP, Baseline: strPtr(p.Baseline), SkippedStep: p.SkippedStep,
		InputTokens: &p.InputTokens, LatencyMs: p.LatencyMS, Error: strPtr(p.Error),
	}
	if len(p.Probabilities) > 0 {
		rec.Probabilities = &p.Probabilities
	}
	if withState {
		rec.State, rec.Questions = p.State, p.Questions
		if len(p.Meta) > 0 {
			rec.Meta = p.Meta
		}
	}
	return rec
}
