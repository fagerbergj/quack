package acp

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRound_EmitsArtifactsAndPlugins: a fresh-session ACP round's
// agent.invoke carries the resolved environment/preamble artifacts (the only
// artifactsrc names an ACP round itself resolves) plus the plugin registry
// rows in scope, on top of its existing sent/received shape.
func TestRound_EmitsArtifactsAndPlugins(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=happy"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		Plugins: func() []ledger.PluginRef { return []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}} },
		PreambleArtifact: func(context.Context) artifactsrc.Artifact {
			return artifactsrc.Artifact{Name: "system/code-implementer", Source: "static", VersionID: "p1"}
		},
		MemoryArtifact: artifactsrc.Artifact{Name: "memory/code-implementer", Source: "static", VersionID: "m1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	envArt := artifactsrc.Artifact{Name: "system/acp.environment", Source: "static", VersionID: "e1"}
	err = a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", envArt, "", "", "", "", func(eventSpec) bool { return true })
	if err != nil {
		t.Fatalf("round: %v", err)
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	var artifacts []ledger.ArtifactRef
	if err := json.Unmarshal([]byte(attrs["quack.artifacts"].AsString()), &artifacts); err != nil {
		t.Fatalf("quack.artifacts unmarshal: %v (raw %s)", err, attrs["quack.artifacts"].AsString())
	}
	want := []ledger.ArtifactRef{
		{Name: "system/acp.environment", Source: "static", VersionID: "e1"},
		{Name: "system/code-implementer", Source: "static", VersionID: "p1"},
		{Name: "memory/code-implementer", Source: "static", VersionID: "m1"},
	}
	if len(artifacts) != 3 || artifacts[0] != want[0] || artifacts[1] != want[1] || artifacts[2] != want[2] {
		t.Errorf("quack.artifacts = %+v, want %+v", artifacts, want)
	}
	if got := attrs["quack.plugins"].AsString(); got != `[{"name":"dotagents","sha":"abc123"}]` {
		t.Errorf("quack.plugins = %q, want the literal wire shape", got)
	}
}
