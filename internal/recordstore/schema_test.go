package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/artifactschema"
)

const nameRequiredSchema = `{"type":"object","required":["name"]}`

func schemaFor(t *testing.T, kind, schema string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{"fake-ext": {kind: json.RawMessage(schema)}})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

func TestSaveBlob_SchemaValid_Succeeds(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.blob", nameRequiredSchema))
	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte(`{"name":"x"}`), "application/json", "h1", Lineage{})
	if err != nil || rev != 1 || id != "test.blob:h1" {
		t.Fatalf("SaveBlob = id=%q rev=%d err=%v, want id=test.blob:h1 rev=1 err=nil", id, rev, err)
	}
}

func TestSaveBlob_SchemaViolation_RefusesAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.blob", nameRequiredSchema))
	_, _, err := c.SaveBlob(ctx, "test.blob", []byte(`{"other":1}`), "application/json", "h1", Lineage{})
	var sv *SchemaViolation
	if !errors.As(err, &sv) {
		t.Fatalf("SaveBlob err = %v, want *SchemaViolation", err)
	}
	if sv.Kind != "test.blob" || len(sv.Violations) == 0 {
		t.Errorf("SchemaViolation = %+v, want Kind=test.blob and a non-empty violation list", sv)
	}
	if _, _, _, _, ok, _ := c.LatestWithMeta(ctx, "test.blob:h1"); ok {
		t.Error("SaveBlob left a revision behind despite the schema violation")
	}
}

func TestSaveBlob_InvalidJSON_Refuses(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.blob", nameRequiredSchema))
	_, _, err := c.SaveBlob(ctx, "test.blob", []byte(`not json at all`), "application/json", "h1", Lineage{})
	var sv *SchemaViolation
	if !errors.As(err, &sv) {
		t.Fatalf("SaveBlob err = %v, want *SchemaViolation for non-JSON content", err)
	}
	if _, _, _, _, ok, _ := c.LatestWithMeta(ctx, "test.blob:h1"); ok {
		t.Error("SaveBlob left a revision behind despite unparseable content")
	}
}

func TestSaveBlob_UnregisteredKindUnaffected(t *testing.T) {
	ctx := context.Background()
	// Registry only knows about test.blob - test.hashed has no declared schema.
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.blob", nameRequiredSchema))
	if _, _, err := c.SaveBlob(ctx, "test.hashed", []byte(`not json, no schema for this kind`), "text/plain", "", Lineage{}); err != nil {
		t.Errorf("SaveBlob(kind with no registered schema) = %v, want success", err)
	}
}

func TestSaveStructured_SchemaViolationBeyondGoValidator_Refuses(t *testing.T) {
	ctx := context.Background()
	// test.structured's own Go Validate only requires "a" - the registered
	// extension schema below requires "c" too, so this proves the extension
	// schema is a second, independent check, not a duplicate of spec.Validate.
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.structured", `{"type":"object","required":["c"]}`))
	_, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "x"}, "main", Lineage{})
	var sv *SchemaViolation
	if !errors.As(err, &sv) {
		t.Fatalf("SaveStructured err = %v, want *SchemaViolation", err)
	}
	if _, _, _, _, ok, _ := c.LatestWithMeta(ctx, "test.structured:main"); ok {
		t.Error("SaveStructured left a revision behind despite the schema violation")
	}
}

func TestEdit_SchemaViolation_RefusesAndPriorRevisionIntact(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t).WithSchemas(schemaFor(t, "test.blob", nameRequiredSchema))
	id, rev, err := c.SaveBlob(ctx, "test.blob", []byte(`{"name":"x"}`), "application/json", "h1", Lineage{})
	if err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	_, _, err = c.Edit(ctx, id, rev, []EditOp{{Old: `"name":"x"`, New: `"other":"y"`}}, Lineage{})
	var sv *SchemaViolation
	if !errors.As(err, &sv) {
		t.Fatalf("Edit err = %v, want *SchemaViolation", err)
	}
	raw, _, _, gotRev, ok, lerr := c.LatestWithMeta(ctx, id)
	if lerr != nil || !ok {
		t.Fatalf("LatestWithMeta after refused edit: ok=%v err=%v", ok, lerr)
	}
	if gotRev != rev || string(raw) != `{"name":"x"}` {
		t.Errorf("after refused edit: revision=%d content=%s, want the prior revision %d/%q untouched", gotRev, raw, rev, `{"name":"x"}`)
	}
}
