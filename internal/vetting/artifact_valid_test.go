package vetting

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/recordstore"
)

const artifactValidTestNodeID = "n1"

func nameRequiredArtifactSchema(t *testing.T, kind string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{
		"fake-ext": {kind: json.RawMessage(`{"type":"object","required":["name"]}`)},
	})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

func artifactValidTestConfig(t *testing.T, schemas *artifactschema.Registry) Config {
	t.Helper()
	return Config{
		Artifact: kindDocument,
		Schemas:  schemas,
		// bestDependencyRevision needs real per-write lineage (NodeID) to scope
		// by, which a plain artifact.InMemoryService() never records.
		Artifacts: newMetaAwareInMemory(),
		User:      "u1",
		ChatID:    "chat-a",
	}
}

func TestArtifactValidCriterion_AbsentWithoutSchema(t *testing.T) {
	cfg := artifactValidTestConfig(t, nil) // no extension declared a schema for kindDocument
	if _, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, time.Time{}); ok {
		t.Error("artifactValidCriterion applies with no registered schema, want it absent")
	}
}

func TestArtifactValidCriterion_AbsentWithoutDeclaredKind(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	cfg.Artifact = "" // node declares no artifact kind at all
	if _, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, time.Time{}); ok {
		t.Error("artifactValidCriterion applies with no declared artifact kind, want it absent")
	}
}

func TestArtifactValidCriterion_FailsWhenNothingWritten(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	c, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, time.Time{})
	if !ok {
		t.Fatal("artifactValidCriterion should apply - kindDocument has a registered schema")
	}
	if c.Score != 0 {
		t.Errorf("Score = %v, want 0 (no artifact exists this run)", c.Score)
	}
	if !strings.Contains(c.Reason, "wrote no artifact") {
		t.Errorf("Reason = %q, want it to say this node wrote no artifact", c.Reason)
	}
}

func TestArtifactValidCriterion_PassesWhenValid(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	c := recordClient(cfg)
	if _, _, err := c.SaveBlob(context.Background(), kindDocument, []byte(`{"name":"trade idea"}`), "application/json", DocumentHint(cfg.ChatID),
		recordstore.Lineage{NodeID: artifactValidTestNodeID}); err != nil {
		t.Fatalf("SaveBlob (test setup): %v", err)
	}
	got, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, time.Time{})
	if !ok {
		t.Fatal("artifactValidCriterion should apply")
	}
	if got.Score != 1 {
		t.Errorf("Score = %v, want 1 (valid artifact exists)", got.Score)
	}
}

func TestArtifactValidCriterion_FailsWithViolationsWhenInvalid(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	c := recordClient(cfg)
	// recordClient carries cfg.Schemas too, so a direct SaveBlob of invalid
	// content is itself refused - bypass enforcement here to set up a
	// pre-existing invalid revision (e.g. written before the schema existed).
	c = c.WithSchemas(nil)
	if _, _, err := c.SaveBlob(context.Background(), kindDocument, []byte(`{"other":1}`), "application/json", DocumentHint(cfg.ChatID),
		recordstore.Lineage{NodeID: artifactValidTestNodeID}); err != nil {
		t.Fatalf("SaveBlob (test setup): %v", err)
	}
	got, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, time.Time{})
	if !ok {
		t.Fatal("artifactValidCriterion should apply")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0", got.Score)
	}
	if !strings.Contains(got.Reason, "required") {
		t.Errorf("Reason = %q, want it to carry the schema violation", got.Reason)
	}
}

// TestArtifactValidCriterion_ScopedToThisNode_IgnoresOtherNodesRevision is
// B3's regression: a chat-scoped id can carry another node's (or an earlier
// run's) valid revision - a node that wrote nothing itself this run must not
// inherit that pass.
func TestArtifactValidCriterion_ScopedToThisNode_IgnoresOtherNodesRevision(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	c := recordClient(cfg)
	if _, _, err := c.SaveBlob(context.Background(), kindDocument, []byte(`{"name":"from an earlier node"}`), "application/json", DocumentHint(cfg.ChatID),
		recordstore.Lineage{NodeID: "old-node"}); err != nil {
		t.Fatalf("SaveBlob (test setup): %v", err)
	}
	got, ok := artifactValidCriterion(context.Background(), cfg, "new-node", time.Time{})
	if !ok {
		t.Fatal("artifactValidCriterion should apply")
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 - new-node wrote nothing, old-node's revision must not count", got.Score)
	}
	if !strings.Contains(got.Reason, "wrote no artifact") {
		t.Errorf("Reason = %q, want it to say this node wrote no artifact", got.Reason)
	}
}

func TestFoldDeterministic_ArtifactValidCriterionIncluded(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	det, _ := computeDeterministicCriteria(context.Background(), "some answer", workerActivity{}, cfg, artifactValidTestNodeID, time.Time{})
	if _, ok := det["artifact_valid"]; !ok {
		t.Fatal("computeDeterministicCriteria: artifact_valid missing for a node with a schema'd declared kind")
	}
	v := mergeDeterministic(verdict{Criteria: map[string]criterionScore{"answers_question": {Score: 1}}}, det, cfg)
	c, ok := v.Criteria["artifact_valid"]
	if !ok || c.Definition == "" || c.Fix == "" {
		t.Errorf("merged artifact_valid criterion = %+v, want a definition/fix stamped", c)
	}
}

func TestFoldDeterministic_ArtifactValidAbsentWithoutDeclaredKind(t *testing.T) {
	cfg := Config{} // no Artifact, no Schemas
	det, _ := computeDeterministicCriteria(context.Background(), "some answer", workerActivity{}, cfg, artifactValidTestNodeID, time.Time{})
	if _, ok := det["artifact_valid"]; ok {
		t.Error("artifact_valid present for a node with no declared artifact kind")
	}
}

// TestSaveEpisodicRound_FallbackNeverStoresInvalidDocument: a schema-violating
// fallback answer falls through to text:<node> instead of landing as the artifact.
func TestSaveEpisodicRound_FallbackNeverStoresInvalidDocument(t *testing.T) {
	svc := newMetaAwareInMemory()
	base := reviewerCfgWithArtifacts(t, svc, true)
	base.IsReviewer = false
	base.Artifact = kindDocument
	base.NodeID = "doc-schema-fallback"
	base.Schemas = nameRequiredArtifactSchema(t, kindDocument)

	prose := "This is the node's plain-text answer, not JSON at all."
	saveEpisodicRound(context.Background(), base, base.NodeID, "turn-1", 1, prose, StagedDelivery{}, nil)

	docID, err := recordstore.IdentityFor(kindDocument, nil, DocumentHint(base.ChatID))
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(base)
	if _, _, ok, err := rc.Latest(context.Background(), docID); err != nil || ok {
		t.Fatalf("document revision exists (ok=%v err=%v), want the schema-invalid fallback to leave no document revision", ok, err)
	}

	textID, err := recordstore.IdentityFor(kindText, nil, base.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, ok, err := rc.Latest(context.Background(), textID)
	if err != nil || !ok || string(raw) != prose {
		t.Fatalf("text:<node> fallback: raw=%q ok=%v err=%v, want the prose answer saved there instead", raw, ok, err)
	}
}

// Node ids are workflow constants and Sleeper chat ids are stable across
// re-runs, so an earlier run's valid artifact must not pass this run.
func TestArtifactValidCriterion_IgnoresAnEarlierRunsRevision(t *testing.T) {
	cfg := artifactValidTestConfig(t, nameRequiredArtifactSchema(t, kindDocument))
	c := recordClient(cfg)
	runStart := time.Now().UTC()
	if _, _, err := c.SaveBlob(context.Background(), kindDocument, []byte(`{"name":"last week"}`), "application/json", DocumentHint(cfg.ChatID),
		recordstore.Lineage{NodeID: artifactValidTestNodeID, SavedAt: runStart.Add(-7 * 24 * time.Hour)}); err != nil {
		t.Fatalf("SaveBlob (test setup): %v", err)
	}
	got, ok := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, runStart)
	if !ok || got.Score != 0 {
		t.Fatalf("score = %v (applies=%v), want 0: the only revision predates this run", got.Score, ok)
	}
	if _, _, err := c.SaveBlob(context.Background(), kindDocument, []byte(`{"name":"this week"}`), "application/json", DocumentHint(cfg.ChatID),
		recordstore.Lineage{NodeID: artifactValidTestNodeID, SavedAt: runStart.Add(time.Second)}); err != nil {
		t.Fatalf("SaveBlob (test setup): %v", err)
	}
	if got, _ := artifactValidCriterion(context.Background(), cfg, artifactValidTestNodeID, runStart); got.Score != 1 {
		t.Fatalf("score = %v, want 1 once this run has written a valid artifact (%s)", got.Score, got.Reason)
	}
}
