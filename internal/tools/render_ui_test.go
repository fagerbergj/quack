package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
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

// turnSpy records the turn_id each save carries (store.TurnAwareService persists it as the row's turn_id).
type turnSpy struct {
	artifact.Service
	mu    sync.Mutex
	turns map[string]string
}

func (s *turnSpy) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, _, _ string, _ []byte, turnID string) (*artifact.SaveResponse, error) {
	s.mu.Lock()
	s.turns[req.FileName] = turnID
	s.mu.Unlock()
	return s.Save(ctx, req)
}

func TestRenderUI_UpsertQuizKeyAndEvents(t *testing.T) {
	spy := &turnSpy{Service: artifact.InMemoryService(), turns: map[string]string{}}
	rc := recordstore.New(spy, "quack", "u1", "chat-a")
	tl, err := Build([]string{"render_ui"}, Deps{RecordStore: rc, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	rt := tl[0].(runnableTool)
	ctx := newArtifactsToolCtx()
	var events []stream.ArtifactRevisionData
	ctx.Ctx = stream.WithYield(stream.WithTurnID(context.Background(), "turn-1"), func(ev stream.SSEEvent) {
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
	if err := json.Unmarshal(raw, &key); err != nil || len(key.Answers) != 3 || key.Answers["q1"].Why == "" || key.Answers["q1"].Label != "It is marked dead immediately" {
		t.Fatalf("quiz key = %+v, %v", key, err)
	}

	if spy.turns["a2ui_surface:pr-412-tutor"] != "turn-1" || spy.turns["quiz_key:pr-412-tutor"] != "turn-1" {
		t.Fatalf("saves carry turn ids %v, want turn-1", spy.turns)
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

// TestRenderUI_ConcurrentUpserts: ADK runs one response's parallel calls
// concurrently; every upsert on the same surface must survive.
func TestRenderUI_ConcurrentUpserts(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
	tl, err := NewRenderUITool(rc, "n1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	const n = 20
	children := make([]any, n)
	for i := range children {
		children[i] = fmt.Sprintf("t%d", i)
	}
	root := map[string]any{"surface_id": "s1", "components": []any{map[string]any{"id": "root", "component": "Column", "children": children}}}
	// Placeholders first so every intermediate surface validates.
	for i := range n {
		root["components"] = append(root["components"].([]any), map[string]any{"id": fmt.Sprintf("t%d", i), "component": "Text", "text": "-"})
	}
	runRenderUI(t, rt, newArtifactsToolCtx(), root)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.Run(newArtifactsToolCtx(), map[string]any{"surface_id": "s1", "components": []any{
				map[string]any{"id": fmt.Sprintf("t%d", i), "component": "Text", "text": fmt.Sprintf("done %d", i)},
			}})
		}()
	}
	wg.Wait()
	raw, _, _, err := rc.Latest(context.Background(), "a2ui_surface:s1")
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if !strings.Contains(string(raw), fmt.Sprintf(`"done %d"`, i)) {
			t.Fatalf("upsert %d lost", i)
		}
	}
}

// TestRenderUI_ResendKeepsGrading: re-sending a graded picker in the model's
// original order without answer_key must keep quiz_key on the right label.
func TestRenderUI_ResendKeepsGrading(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a")
	tl, err := NewRenderUITool(rc, "n1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	args := renderUIFixture(t)
	runRenderUI(t, rt, newArtifactsToolCtx(), args)
	var q1 any
	fresh := renderUIFixture(t)
	for _, c := range fresh["components"].([]any) {
		if c.(map[string]any)["id"] == "q1" {
			q1 = c
		}
	}
	if got := runRenderUI(t, rt, newArtifactsToolCtx(), map[string]any{"surface_id": "pr-412-tutor", "components": []any{q1}}); strings.HasPrefix(got, "VALIDATION_FAILED") {
		t.Fatal(got)
	}
	var s a2ui.Surface
	var key a2ui.QuizKey
	raw, _, _, _ := rc.Latest(context.Background(), "a2ui_surface:pr-412-tutor")
	_ = json.Unmarshal(raw, &s)
	raw, _, _, _ = rc.Latest(context.Background(), "quiz_key:pr-412-tutor")
	_ = json.Unmarshal(raw, &key)
	var got any
	for _, c := range s.Components {
		for _, o := range func() []any { opts, _ := c["options"].([]any); return opts }() {
			if m := o.(map[string]any); c["id"] == "q1" && m["value"] == key.Answers["q1"].Answer {
				got = m["label"]
			}
		}
	}
	if got != "It is marked dead immediately" {
		t.Fatalf("key q1 = %q now points at %v", key.Answers["q1"].Answer, got)
	}
}
