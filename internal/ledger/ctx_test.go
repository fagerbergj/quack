package ledger_test

import (
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestFillBlankCoords_CtxWinsPerField: a field ctx already set is never
// overwritten by the shared stamp, but a blank one is filled - including the
// Artifacts/Plugins lists, which fill or stay exactly like every scalar field.
func TestFillBlankCoords_CtxWinsPerField(t *testing.T) {
	ctxArtifacts := []ledger.ArtifactRef{{Name: "system/code-reviewer", Source: "static", VersionID: "1"}}
	stampArtifacts := []ledger.ArtifactRef{{Name: "system/other-agent", Source: "static", VersionID: "2"}}
	stampPlugins := []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}}

	got := ledger.FillBlankCoords(
		ledger.Coords{Agent: "worker", Artifacts: ctxArtifacts},
		ledger.Coords{Agent: "sibling", Artifacts: stampArtifacts, Plugins: stampPlugins, ChatID: "chat-1"},
	)
	if got.Agent != "worker" {
		t.Errorf("Agent = %q, want worker (ctx must win)", got.Agent)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != ctxArtifacts[0] {
		t.Errorf("Artifacts = %+v, want ctx's own list unchanged", got.Artifacts)
	}
	if got.ChatID != "chat-1" {
		t.Errorf("ChatID = %q, want chat-1 (blank ctx field filled from stamp)", got.ChatID)
	}
	if len(got.Plugins) != 1 || got.Plugins[0] != stampPlugins[0] {
		t.Errorf("Plugins = %+v, want the stamp's list (ctx left it blank)", got.Plugins)
	}
}
