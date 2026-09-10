package memory

import (
	"context"
	"iter"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// fakeModel is a consolidator stub that replies with a fixed text (canned JSON ops).
// usage is nil by default (existing callers get no usage metadata, matching prior behaviour).
type fakeModel struct {
	reply string
	usage *genai.GenerateContentResponseUsageMetadata
}

func (fakeModel) Name() string { return "fake-consolidator" }

func (f fakeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: f.reply}}}, UsageMetadata: f.usage}, nil)
	}
}

func TestCommit_AddThenRecall(t *testing.T) {
	ctx := context.Background()
	consolidator := fakeModel{reply: "```json\n{\"ops\":[{\"action\":\"ADD\",\"content\":\"transportforireland.ie is authoritative for Irish transit\",\"kind\":\"source\"}]}\n```"}
	s := newSQLiteStore(t, "task", consolidator)

	sc := Scope{Role: RoleResearch, User: "u1", Legacy: "web-researcher"}
	n, err := s.Commit(ctx, sc, "web-researcher", Provenance{},
		[]Candidate{{Content: "use the official transit site", Metadata: map[string]string{"kind": "source"}}},
		"Dublin buses run by transportforireland.ie ...")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("Commit wrote %d, want 1", n)
	}

	// Recall goes through the writer's own view (its role + user buckets).
	resp, err := s.View(sc, nil).SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit"})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall got %d, want 1 committed memory", len(resp.Memories))
	}
}

// TestCommit_Noop verifies the gate's vetting drop: when the consolidator returns
// no ops (nothing worth keeping), nothing is written.
func TestCommit_Noop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[]}`})

	n, err := s.Commit(ctx, Scope{Role: RoleResearch, User: "u1"}, "web-researcher", Provenance{}, []Candidate{{Content: "today's bus fare is 2 euro"}}, "")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 0 {
		t.Fatalf("Commit wrote %d, want 0 (vetting should have dropped it)", n)
	}
}

// TestCommit_NoConsolidator guards the read-only-store error path (no LLM call).
func TestCommit_NoConsolidator(t *testing.T) {
	s := &Store{} // no consolidator
	if _, err := s.Commit(context.Background(), Scope{User: "u1"}, "a", Provenance{}, []Candidate{{Content: "x"}}, ""); err == nil {
		t.Fatal("Commit with nil consolidator should error")
	}
}

// TestCommit_ConsolidatorDefaultAgentFillsTokenUsage pins serve.go's openMemory wiring:
// Commit runs from a background goroutine or tool call whose ctx never carries the node's
// coords, so the consolidator's tracedModel needs the SetDefaultAgent("memory") fallback to attribute its token usage at all.
func TestCommit_ConsolidatorDefaultAgentFillsTokenUsage(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}

	reply := fakeModel{
		reply: `{"ops":[{"action":"ADD","content":"fact","kind":"source"}]}`,
		usage: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
	}
	consolidator := inference.TracedModelForTesting(reply, "consolidator-test-model")
	if da, ok := consolidator.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent("memory")
	} else {
		t.Fatal("TracedModelForTesting result does not implement SetDefaultAgent")
	}
	s := newSQLiteStore(t, "task", consolidator)

	// No ledger coords on ctx - mirrors Commit's real callers (background goroutine, tool call).
	if _, err := s.Commit(context.Background(), Scope{Role: RoleResearch, User: "u1"},
		"web-researcher", Provenance{}, []Candidate{{Content: "use the official transit site"}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "gen_ai.client.token.usage" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
			}
			for _, dp := range sum.DataPoints {
				agentVal, _ := dp.Attributes.Value(attribute.Key("agent"))
				if agentVal.AsString() == "memory" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no gen_ai.client.token.usage data point carries agent=memory - the consolidator's SetDefaultAgent fallback never reached the metric")
	}
}

func TestNeighbourProbe(t *testing.T) {
	// Short input passes through untouched.
	if got := neighbourProbe("hello", nil); got != "hello" {
		t.Fatalf("short probe = %q, want hello", got)
	}

	// A long source answer is capped to maxProbeRunes.
	long := strings.Repeat("x", maxProbeRunes*3)
	if got := neighbourProbe(long, nil); len([]rune(got)) != maxProbeRunes {
		t.Fatalf("probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}

	// Staged content leads, so it survives truncation even with a huge answer.
	got := neighbourProbe(strings.Repeat("y", maxProbeRunes*3), []Candidate{{Content: "STAGED-FACT"}})
	if !strings.HasPrefix(got, "STAGED-FACT") {
		t.Fatalf("probe must lead with staged content, got %q...", got[:20])
	}
	if len([]rune(got)) != maxProbeRunes {
		t.Fatalf("capped probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}
}

// TestCommit_AbsorptionMergesThreeDuplicates (epic #1255 P5) end to end: a
// consolidation pass that UPDATEs one memory and DELETEs two others "duplicate of" it must leave the
// survivor carrying all three original ids in absorbed_ids and the summed vote score, with the absorbed two invalidated with reason "absorbed by <survivor>".
func TestCommit_AbsorptionMergesThreeDuplicates(t *testing.T) {
	ctx := context.Background()
	reply := `{"ops":[
		{"action":"UPDATE","id":"survivor","content":"merged: run make test, not go test","kind":"command"},
		{"action":"DELETE","id":"dup1","reason":"duplicate of survivor"},
		{"action":"DELETE","id":"dup2","reason":"duplicate of survivor"}
	]}`
	s := newSQLiteStore(t, "task", fakeModel{reply: reply})
	seedPoint(t, s, point{ID: "survivor", Scope: "role:coding", Content: "run make test", Upvotes: 1, VoteScore: 1})
	seedPoint(t, s, point{ID: "dup1", Scope: "role:coding", Content: "use make test not go test", Upvotes: 1, VoteScore: 1})
	seedPoint(t, s, point{ID: "dup2", Scope: "role:coding", Content: "make test is required", Downvotes: 1, VoteScore: -1})

	n, err := s.Commit(ctx, Scope{Role: RoleCoding}, "consolidator-test", Provenance{}, []Candidate{{Content: "run make test, not go test"}}, "")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 3 {
		t.Fatalf("Commit applied %d ops, want 3 (1 update + 2 invalidate)", n)
	}

	mems, _, err := s.List(ctx, nil, 0, 0, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Memory{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	sv := byID["survivor"]
	sort.Strings(sv.AbsorbedIDs)
	if !reflect.DeepEqual(sv.AbsorbedIDs, []string{"dup1", "dup2"}) {
		t.Fatalf("survivor absorbed_ids = %v, want [dup1 dup2]", sv.AbsorbedIDs)
	}
	// survivor started at +1, dup1 +1, dup2 -1: summed score = 1.
	if sv.Upvotes != 2 || sv.Downvotes != 1 || sv.VoteScore != 1 {
		t.Fatalf("survivor votes = +%d/-%d score %d, want +2/-1 score 1", sv.Upvotes, sv.Downvotes, sv.VoteScore)
	}
	if sv.Content != "merged: run make test, not go test" {
		t.Fatalf("survivor content = %q, want the consolidator's merged wording", sv.Content)
	}
	for _, id := range []string{"dup1", "dup2"} {
		got := byID[id]
		if got.Status != string(StatusInvalidated) || got.InvalidationReason != "absorbed by survivor" {
			t.Fatalf("%s = status %q reason %q, want invalidated / %q", id, got.Status, got.InvalidationReason, "absorbed by survivor")
		}
	}
}

// TestCommit_CandidateCap (issue #1269 item 3): a node that stages more than maxCandidatesPerCommit
// candidates only gets the first three forwarded to consolidation - the rest never even reach the LLM. Run against both backends (#1268's forEachBackend); every op here is a fresh ADD, so there's no fixed id for qdrant's UUID-only point-id to reject.
func TestCommit_CandidateCap(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		reply := `{"ops":[
			{"action":"ADD","content":"one","kind":"convention"},
			{"action":"ADD","content":"two","kind":"convention"},
			{"action":"ADD","content":"three","kind":"convention"}
		]}`
		s := newStore("task", fakeModel{reply: reply})
		staged := []Candidate{
			{Content: "one"}, {Content: "two"}, {Content: "three"}, {Content: "four"}, {Content: "five"},
		}
		n, err := s.Commit(ctx, Scope{Role: RoleCoding}, "chatty-reviewer", Provenance{}, staged, "")
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if n != 3 {
			t.Fatalf("Commit wrote %d, want 3 (capped from 5 staged)", n)
		}
	})
}

// TestConsolidatePrompt_RejectsChangeLog (issue #1269 item 2): given a change-log
// candidate ("X was added in this PR") and a durable convention, a consolidator that follows the
// prompt's instruction (NOOP the change-log one, ADD the convention) must result in exactly one memory written, not two. The fake model is scripted to the desired behavior - it only proves Commit correctly applies a NOOP+ADD response. Run against both backends; the ADD op needs no fixed id.
func TestConsolidatePrompt_RejectsChangeLog(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		reply := `{"ops":[
			{"action":"NOOP"},
			{"action":"ADD","content":"the tier filter is applied index-level, not post-fetch","kind":"convention"}
		]}`
		s := newStore("task", fakeModel{reply: reply})
		staged := []Candidate{
			{Content: "getByID was added to both indexes in this PR"},
			{Content: "the tier filter is applied index-level, not post-fetch"},
		}
		n, err := s.Commit(ctx, Scope{Role: RoleCoding}, "reviewer", Provenance{}, staged, "")
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if n != 1 {
			t.Fatalf("Commit wrote %d, want 1 (change-log NOOPed, convention ADDed)", n)
		}
	})
}

// TestConsolidatePromptTask_MentionsChangeLog pins the prompt text itself (issue
// #1269 item 2) against a future edit silently dropping the change-log rejection
// instruction - a fake-model test can't otherwise catch a regression in prompt wording.
func TestConsolidatePromptTask_MentionsChangeLog(t *testing.T) {
	if !strings.Contains(consolidatePrompts["task"], "CHANGE-LOG") {
		t.Fatal(`consolidatePrompts["task"] no longer mentions rejecting CHANGE-LOG candidates`)
	}
}
