package agent

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
)

// The model joins the event's custom metadata on the wire without writing into the stored event's own map.
func TestCarryModelVersion_KeepsTheEventsCustomMetadata(t *testing.T) {
	key := adka2a.ToA2AMetaKey("custom_metadata")
	ev := &session.Event{}
	ev.CustomMetadata, ev.ModelVersion = map[string]any{"k": "v"}, "m-1"
	out := &a2a.TaskArtifactUpdateEvent{Artifact: &a2a.Artifact{Metadata: map[string]any{key: ev.CustomMetadata}}}
	if err := carryModelVersion(nil, ev, out); err != nil {
		t.Fatal(err)
	}
	got, _ := out.Artifact.Metadata[key].(map[string]any)
	if got[ModelVersionMeta] != "m-1" || got["k"] != "v" {
		t.Errorf("wire custom metadata = %v, want the model and the event's own k", got)
	}
	if _, leaked := ev.CustomMetadata[ModelVersionMeta]; leaked {
		t.Errorf("stored event's CustomMetadata was written: %v", ev.CustomMetadata)
	}
}
