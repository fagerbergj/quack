package artifactschema

import (
	"encoding/json"
	"strings"
	"testing"
)

const nameRequiredSchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["name"],
	"properties": {"name": {"type": "string"}}
}`

func TestBuild_DuplicateKindNamesBothExtensions(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"trade": json.RawMessage(nameRequiredSchema)},
		"ext-b": {"trade": json.RawMessage(nameRequiredSchema)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a kind two extensions both declare")
	}
	for _, want := range []string{"trade", "ext-a", "ext-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_InvalidJSONNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"digest": json.RawMessage(`{not json`)},
	})
	if err == nil {
		t.Fatal("Build: want an error for unparseable schema JSON")
	}
	for _, want := range []string{"ext-a", "digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error = %q, want it to name %q", err, want)
		}
	}
}

func TestBuild_UnresolvableSchemaNamesExtensionAndKind(t *testing.T) {
	_, err := Build(map[string]map[string]json.RawMessage{
		"ext-a": {"retro": json.RawMessage(`{"$ref": "#/$defs/missing"}`)},
	})
	if err == nil {
		t.Fatal("Build: want an error for a schema with a dangling $ref")
	}
	for _, want := range []string{"ext-a", "retro"} {
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
	reg := mustRegistry(t, "digest", nameRequiredSchema)
	if v := reg.Validate("digest", []byte(`{"name": "week 3"}`)); v != nil {
		t.Errorf("Validate valid content = %v, want nil", v)
	}
}

func TestRegistry_ValidateInvalidJSON(t *testing.T) {
	reg := mustRegistry(t, "digest", nameRequiredSchema)
	v := reg.Validate("digest", []byte(`not json`))
	if len(v) != 1 || !strings.Contains(v[0], "not valid JSON") {
		t.Errorf("Validate invalid JSON = %v, want one violation naming invalid JSON", v)
	}
}

func TestRegistry_ValidateSchemaViolation(t *testing.T) {
	reg := mustRegistry(t, "digest", nameRequiredSchema)
	v := reg.Validate("digest", []byte(`{"other": 1}`))
	if len(v) != 1 {
		t.Fatalf("Validate missing-required = %v, want exactly one violation", v)
	}
	if !strings.Contains(v[0], "required") {
		t.Errorf("violation = %q, want it to mention the missing required property", v[0])
	}
}

func TestRegistry_ValidateUnregisteredKind(t *testing.T) {
	reg := mustRegistry(t, "digest", nameRequiredSchema)
	if v := reg.Validate("some-other-kind", []byte(`garbage`)); v != nil {
		t.Errorf("Validate unregistered kind = %v, want nil (untouched)", v)
	}
	if reg.Has("some-other-kind") {
		t.Error("Has(unregistered kind) = true")
	}
}

func TestRegistry_NilSafe(t *testing.T) {
	var reg *Registry
	if reg.Has("digest") {
		t.Error("nil *Registry.Has = true")
	}
	if v := reg.Validate("digest", []byte(`{}`)); v != nil {
		t.Errorf("nil *Registry.Validate = %v, want nil", v)
	}
}

func TestFormatRefusal_CapsViolations(t *testing.T) {
	violations := make([]string, MaxViolationLines+3)
	for i := range violations {
		violations[i] = "violation"
	}
	msg := FormatRefusal("digest", violations)
	if !strings.Contains(msg, `kind "digest"`) {
		t.Errorf("FormatRefusal = %q, want it to name the kind", msg)
	}
	if strings.Count(msg, "- violation") != MaxViolationLines {
		t.Errorf("FormatRefusal shows %d violation lines, want %d", strings.Count(msg, "- violation"), MaxViolationLines)
	}
	if !strings.Contains(msg, "...and 3 more") {
		t.Errorf("FormatRefusal = %q, want a trailing count of the remaining 3", msg)
	}
	if !strings.Contains(msg, "call the tool again") {
		t.Errorf("FormatRefusal = %q, want it to tell the model to retry", msg)
	}
}

func TestFormatRefusal_NoCapUnderLimit(t *testing.T) {
	msg := FormatRefusal("digest", []string{"path: problem"})
	if strings.Contains(msg, "more") {
		t.Errorf("FormatRefusal under the cap = %q, want no '...and N more'", msg)
	}
}
