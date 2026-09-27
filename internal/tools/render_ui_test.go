package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/a2ui"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

func renderUIFixture(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../a2ui/testdata/example.json")
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func runRenderUI(t *testing.T, rt runnableTool, ctx artifactsToolCtx, args map[string]any) string {
	t.Helper()
	out, err := rt.Run(ctx, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s, _ := out["result"].(string)
	return s
}

func TestRenderUI_UpsertQuizKeyAndEvents(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
	tl, err := Build([]string{"render_ui"}, Deps{RecordStore: rc, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	rt := tl[0].(runnableTool)
	ctx := newArtifactsToolCtx()
	var events []stream.ArtifactRevisionData
	ctx.Ctx = stream.WithYield(context.Background(), func(ev stream.SSEEvent) {
		if ev.Name == stream.EventArtifactRevision {
			events = append(events, ev.Data.(stream.ArtifactRevisionData))
		}
	})

	// First call: components/data_model arrive stringified, as some models send them.
	args := renderUIFixture(t)
	for _, k := range []string{"components", "data_model"} {
		b, _ := json.Marshal(args[k])
		args[k] = string(b)
	}
	var res renderUIResult
	if err := json.Unmarshal([]byte(runRenderUI(t, rt, ctx, args)), &res); err != nil {
		t.Fatal(err)
	}
	if res.ArtifactID != "a2ui_surface:pr-412-tutor" || res.Revision != 1 {
		t.Fatalf("first render = %+v", res)
	}

	// Second call: one changed Text and one new one, no answer key.
	upd := map[string]any{"surface_id": "pr-412-tutor", "components": []any{
		map[string]any{"id": "submit_label", "component": "Text", "text": "Score: 2/3"},
		map[string]any{"id": "q1_mark", "component": "Text", "text": "Correct"},
		map[string]any{"id": "quiz", "component": "Column", "children": []any{"q1", "q1_mark", "q2", "q3", "submit"}},
	}}
	if err := json.Unmarshal([]byte(runRenderUI(t, rt, ctx, upd)), &res); err != nil || res.Revision != 2 {
		t.Fatalf("second render = %+v, %v", res, err)
	}

	raw, _, _, err := rc.Latest(context.Background(), res.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	var s a2ui.Surface
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Components) != 18 || s.DataModel["answers"] == nil {
		t.Fatalf("merged surface: %d components, data_model %v", len(s.Components), s.DataModel)
	}
	if last := s.Components[len(s.Components)-1]; last["id"] != "q1_mark" {
		t.Fatalf("new component not appended: %v", last)
	}

	raw, _, found, err := rc.Latest(context.Background(), "quiz_key:pr-412-tutor")
	if err != nil || !found {
		t.Fatalf("quiz key: found=%v err=%v", found, err)
	}
	var key a2ui.QuizKey
	if err := json.Unmarshal(raw, &key); err != nil || len(key.Answers) != 3 || key.Answers["q1"].Why == "" {
		t.Fatalf("quiz key = %+v, %v", key, err)
	}

	want := []string{"a2ui_surface:pr-412-tutor#1", "quiz_key:pr-412-tutor#1", "a2ui_surface:pr-412-tutor#2"}
	var got []string
	for _, e := range events {
		got = append(got, e.ID+"#"+string(rune('0'+e.Revision)))
		if e.NodeID != "n1" {
			t.Errorf("event node_id = %q", e.NodeID)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact_revision events = %v, want %v", got, want)
	}
}

func TestRenderUI_ValidationFailedIsToolOutput(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
	tl, err := NewRenderUITool(rc, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	cases := map[string]map[string]any{
		"dangling ref": {"surface_id": "s1", "components": []any{map[string]any{"id": "root", "component": "Card", "child": "missing"}}},
		"bad id":       {"surface_id": "a/b", "components": []any{map[string]any{"id": "root", "component": "Text", "text": "x"}}},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if got := runRenderUI(t, rt, newArtifactsToolCtx(), args); !strings.HasPrefix(got, "VALIDATION_FAILED: ") {
				t.Fatalf("result = %q", got)
			}
		})
	}
	if _, _, found, _ := rc.Latest(context.Background(), "a2ui_surface:s1"); found {
		t.Fatal("an invalid surface was saved")
	}

	nilStore, err := NewRenderUITool(nil, "n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nilStore.(runnableTool).Run(newArtifactsToolCtx(), cases["dangling ref"]); err == nil {
		t.Fatal("nil record store should error")
	}
}
