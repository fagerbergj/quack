// artifactschema_boot_test.go: buildArtifactSchemas collects every built SDK
// extension's declared artifact schemas at boot - a duplicate kind across two
// extensions, or a schema that fails to compile, must fail boot naming the
// culprits, not silently pick one or skip the kind.
package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// fakeSchemaExtension implements extsdk.Extension + extsdk.ArtifactSchemas -
// a stand-in for a real SDK module (mirrors fakeUIExtension), without
// depending on the Sleeper module in this unit test.
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

// TestBuildArtifactSchemas_IgnoresExtensionsWithoutTheInterface: a module
// implementing no sdk.ArtifactSchemas (e.g. fakeUIExtension) contributes
// nothing and causes no error - the interface stays optional.
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

// panickingSchemaExtension implements extsdk.ArtifactSchemas by panicking -
// an extension bug (e.g. sleeper's own ArtifactSchemas reading a missing
// embedded file) must fail boot with a named error, not crash with a stack.
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
