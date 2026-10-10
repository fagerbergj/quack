// buildArtifactSchemas must fail boot naming the culprits on a duplicate kind across extensions
// or a schema that fails to compile, never silently pick one or skip the kind.
package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// fakeSchemaExtension stands in for an SDK module declaring artifact schemas, without depending on Sleeper.
type fakeSchemaExtension struct {
	schemas map[string]json.RawMessage
}

func (fakeSchemaExtension) Tools() []tool.Tool                       { return nil }
func (fakeSchemaExtension) RegisterRoutes(authed, public chi.Router) {}
func (e fakeSchemaExtension) ArtifactSchemas() map[string]json.RawMessage {
	return e.schemas
}

func TestBuildArtifactSchemas_DuplicateKindNamesBothExtensions(t *testing.T) {
	exts := []builtSDKExtension{
		{name: "ext-a", ext: fakeSchemaExtension{schemas: map[string]json.RawMessage{
			"trade-finder": json.RawMessage(`{"type":"object"}`),
		}}},
		{name: "ext-b", ext: fakeSchemaExtension{schemas: map[string]json.RawMessage{
			"trade-finder": json.RawMessage(`{"type":"object"}`),
		}}},
	}
	if _, err := buildArtifactSchemas(exts); err == nil {
		t.Fatal("buildArtifactSchemas: want an error for a kind two extensions both declare")
	} else {
		for _, want := range []string{"trade-finder", "ext-a", "ext-b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("buildArtifactSchemas error = %q, want it to name %q", err, want)
			}
		}
	}
}

func TestBuildArtifactSchemas_UncompilableSchemaNamesExtensionAndKind(t *testing.T) {
	exts := []builtSDKExtension{
		{name: "ext-a", ext: fakeSchemaExtension{schemas: map[string]json.RawMessage{
			"digest": json.RawMessage(`{not valid json`),
		}}},
	}
	err := func() error { _, err := buildArtifactSchemas(exts); return err }()
	if err == nil {
		t.Fatal("buildArtifactSchemas: want an error for a schema that fails to compile")
	}
	for _, want := range []string{"ext-a", "digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("buildArtifactSchemas error = %q, want it to name %q", err, want)
		}
	}
}

// TestBuildArtifactSchemas_IgnoresExtensionsWithoutTheInterface: sdk.ArtifactSchemas stays optional.
func TestBuildArtifactSchemas_IgnoresExtensionsWithoutTheInterface(t *testing.T) {
	exts := []builtSDKExtension{{name: "fake-ui-test", ext: fakeUIExtension{}}}
	reg, err := buildArtifactSchemas(exts)
	if err != nil {
		t.Fatalf("buildArtifactSchemas: %v", err)
	}
	if reg.Has("anything") {
		t.Error("registry built from a non-ArtifactSchemas extension reports a kind registered")
	}
}

// panickingSchemaExtension: an extension bug in ArtifactSchemas must fail boot with a named error,
// not crash with a stack.
type panickingSchemaExtension struct{}

func (panickingSchemaExtension) Tools() []tool.Tool                       { return nil }
func (panickingSchemaExtension) RegisterRoutes(authed, public chi.Router) {}
func (panickingSchemaExtension) ArtifactSchemas() map[string]json.RawMessage {
	panic("embedded schema file missing")
}

func TestBuildArtifactSchemas_RecoversExtensionPanicIntoNamedError(t *testing.T) {
	exts := []builtSDKExtension{{name: "ext-panics", ext: panickingSchemaExtension{}}}
	_, err := buildArtifactSchemas(exts)
	if err == nil {
		t.Fatal("buildArtifactSchemas: want an error, not a propagated panic")
	}
	for _, want := range []string{"ext-panics", "embedded schema file missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("buildArtifactSchemas error = %q, want it to contain %q", err, want)
		}
	}
}
