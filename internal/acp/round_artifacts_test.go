package acp

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRound_EmitsArtifactsAndPlugins: a fresh-session round's agent.invoke carries the resolved
// environment/preamble artifacts plus the plugin rows in scope.
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
		Command:  []string{os.Args[0]},
		Env:      []string{"QUACK_ACP_FAKE=happy"},
		Home:     t.TempDir(),
		Jail:     jail,
		UserID:   "u1",
		Plugins:  func() []ledger.PluginRef { return []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}} },
		Preamble: func(context.Context) string { return "you are a coder" },
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

	attrs := invokeAttrs(t, capExp.records)
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

// TestRound_OmitsPreambleArtifactsWhenNoPreambleSent: roundArtifacts gates on a preamble actually going out
// this round (sentPreamble), the same condition that prepends it.
func TestRound_OmitsPreambleArtifactsWhenNoPreambleSent(t *testing.T) {
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
		// No Preamble set - steerHooks never prepends one, so sentPreamble stays false
		// even though both artifacts below are wired.
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

	attrs := invokeAttrs(t, capExp.records)
	var artifacts []ledger.ArtifactRef
	if err := json.Unmarshal([]byte(attrs["quack.artifacts"].AsString()), &artifacts); err != nil {
		t.Fatalf("quack.artifacts unmarshal: %v (raw %s)", err, attrs["quack.artifacts"].AsString())
	}
	want := []ledger.ArtifactRef{{Name: "system/acp.environment", Source: "static", VersionID: "e1"}}
	if len(artifacts) != 1 || artifacts[0] != want[0] {
		t.Errorf("quack.artifacts = %+v, want %+v (no preamble sent, so no preamble/memory artifact)", artifacts, want)
	}
}

// invokeAttrs is the round's one invoke_agent record; its tool.call records ride the same logger.
func invokeAttrs(t *testing.T, records []sdklog.Record) map[string]attribute.Value {
	t.Helper()
	out := invokeRecords(records)
	if len(out) != 1 {
		t.Fatalf("got %d invoke_agent records, want 1", len(out))
	}
	return out[0]
}

// invokeRecords is every invoke_agent record's attributes, in emit order.
func invokeRecords(records []sdklog.Record) []map[string]attribute.Value {
	var out []map[string]attribute.Value
	for _, r := range records {
		attrs := map[string]attribute.Value{}
		r.WalkAttributes(func(kv attribute.KeyValue) bool { attrs[string(kv.Key)] = kv.Value; return true })
		if attrs[otelobs.GenAIOperationName].AsString() == otelobs.GenAIOperationInvokeAgent {
			out = append(out, attrs)
		}
	}
	return out
}

// A reused (pinned) round sends no preamble but still runs under the one its session opened with, so its
// agent.invoke keeps that provenance: a judge revision's answer must still name the prompt version it ran on.
func TestRound_PinnedRoundKeepsSessionPreambleArtifacts(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:  []string{os.Args[0]},
		Env:      []string{"QUACK_ACP_FAKE=echo"},
		Home:     t.TempDir(),
		Jail:     jail,
		UserID:   "u1",
		Preamble: func(context.Context) string { return "you are a coder" },
		PreambleArtifact: func(context.Context) artifactsrc.Artifact {
			return artifactsrc.Artifact{Name: "system/code-implementer", Source: "langfuse", VersionID: "3"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "tok-pinned-provenance"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)
	envArt := artifactsrc.Artifact{Name: "system/acp.environment", Source: "static", VersionID: "e1"}
	for range 2 {
		if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", envArt, "", "", token, "", func(eventSpec) bool { return true }); err != nil {
			t.Fatalf("round: %v", err)
		}
	}
	if _, ok := pinned.Load(token); !ok {
		t.Fatal("round 2 did not run on the pinned process")
	}

	want := []ledger.ArtifactRef{{Name: "system/acp.environment", Source: "static", VersionID: "e1"}, {Name: "system/code-implementer", Source: "langfuse", VersionID: "3"}}
	rounds := invokeRecords(capExp.records)
	if len(rounds) != 2 {
		t.Fatalf("got %d invoke_agent records, want one per round", len(rounds))
	}
	for i, attrs := range rounds {
		raw := attrs["quack.artifacts"].AsString()
		var got []ledger.ArtifactRef
		if err := json.Unmarshal([]byte(raw), &got); err != nil || !slices.Equal(got, want) {
			t.Errorf("round %d quack.artifacts = %s (err %v), want %+v", i+1, raw, err, want)
		}
	}
}
