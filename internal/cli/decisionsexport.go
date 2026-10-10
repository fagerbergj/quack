package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/schema"
)

// DecisionDatasetName is the Langfuse dataset that holds one point's decisions.
func DecisionDatasetName(point string) string { return "decisions/" + point }

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// DecisionItemID keys a dataset item by chat, node, round, point and a hash of the
// model's input state, so re-exporting the same decision updates one item in place.
func DecisionItemID(r schema.DecisionRecord) string {
	state, _ := json.Marshal(r.State)
	return shortHash(r.ChatId, deref(r.NodeId), deref(r.Round), r.Point, shortHash(string(state)))
}

type decisionItemInput struct {
	State     any `json:"state"`
	Questions any `json:"questions"`
}

type decisionOutput struct {
	Top           string                        `json:"top"`
	TopP          float64                       `json:"top_p"`
	Answers       map[string]map[string]float64 `json:"answers,omitempty"`
	Outcome       string                        `json:"outcome"`
	Confident     bool                          `json:"confident"`
	Handler       string                        `json:"handler"`
	Mode          string                        `json:"mode"`
	QuackVersion  string                        `json:"quack_version"`
	DatasetItemID string                        `json:"dataset_item_id"`
}

// DecisionExportSummary counts what ExportDecisions wrote, per point.
type DecisionExportSummary struct {
	Point   string   `json:"point"`
	Dataset string   `json:"dataset"`
	Items   int      `json:"items"`
	Runs    []string `json:"runs"`
}

// ExportDecisions upserts one dataset per point and a run item per decision under
// <handler>@<version>; ids are deterministic, so a re-export converges.
func ExportDecisions(ctx context.Context, lf *langfuse.Client, version string, recs []schema.DecisionRecord) ([]DecisionExportSummary, error) {
	items := map[string]schema.DecisionRecord{}
	for _, r := range recs {
		if deref(r.Baseline) != "" {
			items[DecisionItemID(r)] = r // the last entry for a key wins
		}
	}
	ids := slices.Sorted(maps.Keys(items))
	sums := map[string]*DecisionExportSummary{}
	ensured := map[string]bool{}
	var order []string
	for _, id := range ids {
		r := items[id]
		ds := DecisionDatasetName(r.Point)
		if !ensured[ds] {
			if err := ensureDataset(ctx, lf, ds); err != nil {
				return nil, err
			}
			ensured[ds] = true
		}
		if err := exportDecision(ctx, lf, version, id, ds, r); err != nil {
			return nil, err
		}
		s := sums[r.Point]
		if s == nil {
			s = &DecisionExportSummary{Point: r.Point, Dataset: ds}
			sums[r.Point], order = s, append(order, r.Point)
		}
		if run := r.Handler + "@" + version; !slices.Contains(s.Runs, run) {
			s.Runs = append(s.Runs, run)
		}
		s.Items++
	}
	out := make([]DecisionExportSummary, 0, len(order))
	for _, p := range order {
		out = append(out, *sums[p])
	}
	return out, nil
}

func exportDecision(ctx context.Context, lf *langfuse.Client, version, id, dataset string, r schema.DecisionRecord) error {
	meta := map[string]any{"chat": r.ChatId, "node": deref(r.NodeId), "round": deref(r.Round), "quack_version": version, "handler": r.Handler, "mode": r.Mode}
	if r.Meta != nil {
		meta["state_meta"] = r.Meta
	}
	req := langfuse.CreateDatasetItemRequest{
		DatasetName: dataset, ID: id, Metadata: meta,
		Input:          decisionItemInput{State: r.State, Questions: r.Questions},
		ExpectedOutput: map[string]string{"baseline": deref(r.Baseline)},
	}
	if err := lf.CreateDatasetItem(ctx, req); err != nil {
		return fmt.Errorf("decisions export: create item %s: %w", id, err)
	}
	runName := r.Handler + "@" + version
	trace := shortHash(runName, id)
	if err := lf.Ingest(ctx, decisionTraceEvents(trace, id, version, r)); err != nil {
		return fmt.Errorf("decisions export: trace for item %s: %w", id, err)
	}
	ri := langfuse.CreateDatasetRunItemRequest{DatasetItemID: id, RunName: runName, TraceID: trace}
	if err := lf.CreateDatasetRunItem(ctx, ri); err != nil {
		return fmt.Errorf("decisions export: run item %s: %w", id, err)
	}
	return nil
}

// decisionTraceEvents is the trace carrying the model's decision plus its scores; an
// unanswered call (unavailable) gets no scores, since a 0 would read as disagreement.
func decisionTraceEvents(trace, itemID, version string, r schema.DecisionRecord) []langfuse.IngestEvent {
	ts := r.At.UTC().Format(time.RFC3339Nano)
	out := decisionOutput{
		Top: deref(r.Top), TopP: deref(r.TopP), Answers: deref(r.Probabilities), Outcome: r.Outcome, Confident: r.Confident,
		Handler: r.Handler, Mode: r.Mode, QuackVersion: version, DatasetItemID: itemID,
	}
	events := []langfuse.IngestEvent{{ID: trace + "-trace", Type: "trace-create", Timestamp: ts,
		Body: map[string]any{"id": trace, "name": "decision/" + r.Point, "timestamp": ts, "output": out,
			"input": decisionItemInput{State: r.State, Questions: r.Questions}}}}
	if out.Top == "" {
		return events
	}
	score := func(name string, v float64) langfuse.IngestEvent {
		return langfuse.IngestEvent{ID: trace + "-" + name, Type: "score-create", Timestamp: ts,
			Body: map[string]any{"id": trace + "-" + name, "traceId": trace, "name": name, "value": v, "dataType": "NUMERIC"}}
	}
	agrees := 0.0
	if out.Top == deref(r.Baseline) {
		agrees = 1
	}
	return append(events, score("agrees_with_baseline", agrees), score("top_p", out.TopP))
}

// RunDecisionsExport is `quack decisions export --langfuse`: fetches decisions from the
// server and writes them to Langfuse with the given clients.
func RunDecisionsExport(ctx context.Context, w io.Writer, server string, f DecisionFilter, lf *langfuse.Client) error {
	list, err := fetchDecisions(ctx, server, f, true)
	if err != nil {
		return err
	}
	sums, err := ExportDecisions(ctx, lf, list.QuackVersion, list.Data)
	if err != nil {
		return err
	}
	if len(sums) == 0 {
		_, err = fmt.Fprintln(w, "No decisions with a baseline to export.")
		return err
	}
	for _, s := range sums {
		fmt.Fprintf(w, "%s: %d item(s), runs %s\n", s.Dataset, s.Items, strings.Join(s.Runs, ", "))
	}
	return nil
}
