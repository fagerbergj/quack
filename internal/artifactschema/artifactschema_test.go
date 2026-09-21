package artifactschema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/recordstore"
)

const nameRequiredSchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["name"],
	"properties": {"name": {"type": "string"}}
}`

// testIdentity: instance = hint verbatim - the same shape sleeperkinds' real
// kinds use, so these test-registered kinds behave like a real extension's.
func testIdentity(_ []byte, hint string) (string, error) { return hint, nil }

func init() {
	for _, kind := range []string{"test-kind-a", "test-kind-b", "test-kind-c", "test-kind-d"} {
		recordstore.Register(kind, recordstore.KindSpec{Class: recordstore.Blob, Identity: testIdentity, RequiresHint: true})
	}
	recordstore.Register("test-system-kind", recordstore.KindSpec{Class: recordstore.Blob, Identity: testIdentity, System: true})
	recordstore.Register("test-content-hash-kind", recordstore.KindSpec{Class: recordstore.Blob, Identity: testIdentity})
}

func TestBuild_DuplicateKindNamesBothExtensions(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"test-kind-a": json.RawMessage(nameRequiredSchema)},
		"ext-b": {"test-kind-a": json.RawMessage(nameRequiredSchema)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a kind two extensions both declare")
	}
	for _, want := range []string{"test-kind-a", "ext-a", "ext-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_InvalidJSONNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"test-kind-b": json.RawMessage(`{not json`)},
	})
	if err == nil {
		t.Fatal("Build: want an error for unparseable schema JSON")
	}
	for _, want := range []string{"ext-a", "test-kind-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_UnresolvableSchemaNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"test-kind-c": json.RawMessage(`{"$ref": "#/$defs/missing"}`)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a schema with a dangling $ref")
	}
	for _, want := range []string{"ext-a", "test-kind-c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_UnregisteredKindNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"trade_finder_typo": json.RawMessage(nameRequiredSchema)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a kind recordstore does not know")
	}
	for _, want := range []string{"ext-a", "trade_finder_typo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_SystemKindNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"test-system-kind": json.RawMessage(nameRequiredSchema)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a System kind (reserved for internal writes)")
	}
	for _, want := range []string{"ext-a", "test-system-kind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_NoSchemasReturnsNilRegistry(t *testing.T) {
	reg, err := Build(map[string]map[string]json.RawMessage{"ext-a": {}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if reg.Has("anything") {
		t.Error("empty Build result reports a kind registered")
	}
	if reg.Validate("anything", []byte(`{}`)) != nil {
		t.Error("empty Build result reports a violation")
	}
}

func mustRegistry(t *testing.T, kind, schema string) *Registry {
	t.Helper()
	reg, err := Build(map[string]map[string]json.RawMessage{"ext-a": {kind: json.RawMessage(schema)}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return reg
}

func TestRegistry_ValidateValidContent(t *testing.T) {
	reg := mustRegistry(t, "test-kind-d", nameRequiredSchema)
	if v := reg.Validate("test-kind-d", []byte(`{"name": "week 3"}`)); v != nil {
		t.Errorf("Validate valid content = %v, want nil", v)
	}
}

func TestRegistry_ValidateInvalidJSON(t *testing.T) {
	reg := mustRegistry(t, "test-kind-d", nameRequiredSchema)
	v := reg.Validate("test-kind-d", []byte(`not json`))
	if len(v) != 1 || !strings.Contains(v[0], "not valid JSON") {
		t.Errorf("Validate invalid JSON = %v, want one violation naming invalid JSON", v)
	}
}

func TestRegistry_ValidateSchemaViolation(t *testing.T) {
	reg := mustRegistry(t, "test-kind-d", nameRequiredSchema)
	v := reg.Validate("test-kind-d", []byte(`{"other": 1}`))
	if len(v) != 1 {
		t.Fatalf("Validate missing-required = %v, want exactly one violation", v)
	}
	if !strings.Contains(v[0], "required") {
		t.Errorf("violation = %q, want it to mention the missing required property", v[0])
	}
}

func TestRegistry_ValidateUnregisteredKind(t *testing.T) {
	reg := mustRegistry(t, "test-kind-d", nameRequiredSchema)
	if v := reg.Validate("some-other-kind", []byte(`garbage`)); v != nil {
		t.Errorf("Validate unregistered kind = %v, want nil (untouched)", v)
	}
	if reg.Has("some-other-kind") {
		t.Error("Has(unregistered kind) = true")
	}
}

func TestRegistry_NilSafe(t *testing.T) {
	var reg *Registry
	if reg.Has("test-kind-d") {
		t.Error("nil *Registry.Has = true")
	}
	if v := reg.Validate("test-kind-d", []byte(`{}`)); v != nil {
		t.Errorf("nil *Registry.Validate = %v, want nil", v)
	}
}

func TestFormatRefusal_NamesKindAndListsEachViolation(t *testing.T) {
	violations := []string{"root: violation one", "root: violation two"}
	msg := FormatRefusal("test-kind-d", violations, nil)
	if !strings.Contains(msg, `kind "test-kind-d"`) {
		t.Errorf("FormatRefusal = %q, want it to name the kind", msg)
	}
	for _, v := range violations {
		if !strings.Contains(msg, "- "+v) {
			t.Errorf("FormatRefusal = %q, want it to list %q", msg, v)
		}
	}
	if !strings.Contains(msg, "call the tool again") {
		t.Errorf("FormatRefusal = %q, want it to tell the model to retry", msg)
	}
}

func TestFormatRefusal_CarriesTheSchema(t *testing.T) {
	msg := FormatRefusal("test-kind-d", []string{"path: problem"}, json.RawMessage(`{"type":"object"}`))
	if !strings.Contains(msg, `{"type":"object"}`) {
		t.Errorf("FormatRefusal = %q, want it to carry the schema", msg)
	}
}

// A schema on quack's own kinds could only break quack's own writes: the
// fallback kinds a refused answer lands in, and gate-only structured kinds.
func TestBuild_RefusesFallbackAndGateOnlyKinds(t *testing.T) {
	for _, kind := range []string{"text", "bytes", "judge_round"} {
		_, err := Build(map[string]map[string]json.RawMessage{"ext-a": {kind: json.RawMessage(nameRequiredSchema)}})
		if err == nil || !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "ext-a") {
			t.Errorf("Build(%q) err = %v, want a refusal naming the extension and kind", kind, err)
		}
	}
}

// A kind stored under a content hash cannot be found by the chat's hint id,
// so a schema on it would fail artifact_valid on every run.
func TestBuild_RefusesAKindThatIsNotHintIdentified(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{"ext-a": {"test-content-hash-kind": json.RawMessage(nameRequiredSchema)}})
	if err == nil || !strings.Contains(err.Error(), "test-content-hash-kind") || !strings.Contains(err.Error(), "ext-a") {
		t.Fatalf("Build err = %v, want a refusal naming the extension and kind", err)
	}
}
