package replay

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// entry is a small builder for one hand-crafted ledger line, so these tests
// exercise Session directly without going through the full emission path
// (that round trip is replaytest's job).
type entry struct {
	ts    time.Time
	attrs map[string]any
}

func chat(ts time.Time, node, agent, round, model string, extra map[string]any) entry {
	a := map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.request.model":  model,
		"quack.node":            node,
		"gen_ai.agent.name":     agent,
		"quack.round":           round,
	}
	for k, v := range extra {
		a[k] = v
	}
	return entry{ts: ts, attrs: a}
}

func execTool(ts time.Time, node, agent, round, tool string, result map[string]any) entry {
	resultJSON, _ := json.Marshal(result)
	return entry{ts: ts, attrs: map[string]any{
		"gen_ai.operation.name":   "execute_tool",
		"gen_ai.tool.name":        tool,
		"gen_ai.tool.call.result": string(resultJSON),
		"quack.node":              node,
		"gen_ai.agent.name":       agent,
		"quack.round":             round,
	}}
}

func writeJSONL(t *testing.T, entries []entry) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entries.jsonl")
	var sb strings.Builder
	for _, e := range entries {
		l := line{Timestamp: e.ts, Attrs: e.attrs}
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

func writeZip(t *testing.T, entries []entry, manifest Manifest) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	mf, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatalf("create manifest.json: %v", err)
	}
	mb, _ := json.Marshal(manifest)
	if _, err := mf.Write(mb); err != nil {
		t.Fatalf("write manifest.json: %v", err)
	}

	ef, err := zw.Create("entries.jsonl")
	if err != nil {
		t.Fatalf("create entries.jsonl: %v", err)
	}
	for _, e := range entries {
		l := line{Timestamp: e.ts, Attrs: e.attrs}
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		ef.Write(b)
		ef.Write([]byte("\n"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return path
}

func t0() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestNextChat_HappyPath(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"hello"}]}`,
			"gen_ai.response.model":  "worker-model-v1",
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	resp, err := sess.NextChat(coords, "worker-model", nil)
	if err != nil {
		t.Fatalf("NextChat: %v", err)
	}
	if resp.Content == nil || resp.Content.Parts[0].Text != "hello" {
		t.Errorf("resp.Content = %+v, want text %q", resp.Content, "hello")
	}
	if resp.ModelVersion != "worker-model-v1" {
		t.Errorf("ModelVersion = %q", resp.ModelVersion)
	}
	if !resp.TurnComplete {
		t.Errorf("TurnComplete = false, want true")
	}

	rep := sess.Report()
	if !rep.Clean() {
		t.Errorf("Report = %+v, want clean", rep)
	}
}

func TestNextChat_ExtraCall(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	if _, err := sess.NextChat(coords, "worker-model", nil); err != nil {
		t.Fatalf("first NextChat: %v", err)
	}
	_, err = sess.NextChat(coords, "worker-model", nil)
	if err == nil {
		t.Fatalf("second NextChat: want an extra-call error, got nil")
	}
	me, ok := err.(*MissError)
	if !ok {
		t.Fatalf("err type = %T, want *MissError", err)
	}
	if me.Class != ClassExtra {
		t.Errorf("Class = %q, want extra", me.Class)
	}
	if me.Position != 1 {
		t.Errorf("Position = %d, want 1", me.Position)
	}

	rep := sess.Report()
	if len(rep.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1", len(rep.Failures))
	}
	if rep.Clean() {
		t.Errorf("Report.Clean() = true, want false")
	}
}

// TestNextChat_ExtraCall_NeverRecorded pins the "gate makes an extra call"
// acceptance case: a stream nothing was ever recorded for still fails
// loudly (not a panic / nil-map access), at position 0.
func TestNextChat_ExtraCall_NeverRecorded(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "judge", Round: "judge-r7"}
	_, err = sess.NextChat(coords, "judge-model", nil)
	me, ok := err.(*MissError)
	if !ok {
		t.Fatalf("err type = %T, want *MissError", err)
	}
	if me.Class != ClassExtra || me.Position != 0 || len(me.Diff) != 0 {
		t.Errorf("got %+v, want extra/pos0/empty-diff", me)
	}
}

func TestNextChat_Mismatched(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model-A", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	_, err = sess.NextChat(coords, "worker-model-B", nil)
	me, ok := err.(*MissError)
	if !ok {
		t.Fatalf("err type = %T, want *MissError", err)
	}
	if me.Class != ClassMismatched {
		t.Errorf("Class = %q, want mismatched", me.Class)
	}
	if len(me.Diff) == 0 || me.Diff[0].Name != "worker-model-A" {
		t.Errorf("Diff = %+v, want the recorded model name", me.Diff)
	}
}

func TestNextChat_PromptDriftIsInformationalNotFatal(t *testing.T) {
	sysBytes := []byte(`{"role":"system","parts":[{"text":"you are a test agent"}]}`)
	recordedHash := contentHash(sysBytes)
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.prompt.version": recordedHash,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}

	// Same system instruction: no drift.
	if _, err := sess.NextChat(coords, "worker-model", sysBytes); err != nil {
		t.Fatalf("NextChat: %v", err)
	}
	if rep := sess.Report(); len(rep.Drift) != 0 {
		t.Errorf("Drift = %+v, want none for an identical system instruction", rep.Drift)
	}
}

func TestNextChat_PromptDriftRecordedOnEdit(t *testing.T) {
	sysBytes := []byte(`{"role":"system","parts":[{"text":"you are a test agent"}]}`)
	recordedHash := contentHash(sysBytes)
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.prompt.version": recordedHash,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}

	edited := []byte(`{"role":"system","parts":[{"text":"you are a DIFFERENT test agent"}]}`)
	if _, err := sess.NextChat(coords, "worker-model", edited); err != nil {
		t.Fatalf("NextChat with an edited prompt should still replay green: %v", err)
	}
	rep := sess.Report()
	if len(rep.Drift) != 1 {
		t.Fatalf("Drift = %+v, want exactly 1", rep.Drift)
	}
	if rep.Drift[0].Recorded != recordedHash {
		t.Errorf("Drift.Recorded = %q, want %q", rep.Drift[0].Recorded, recordedHash)
	}
	if rep.Drift[0].Live == recordedHash {
		t.Errorf("Drift.Live should differ from Recorded")
	}
	if len(rep.Failures) != 0 {
		t.Errorf("Failures = %+v, want none - prompt drift is informational only", rep.Failures)
	}
}

func TestNextToolResult_HappyPathAndExtra(t *testing.T) {
	path := writeJSONL(t, []entry{
		execTool(t0(), "node-a", "worker", "worker-r0", "web_search", map[string]any{"results": []any{"a"}}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	res, err := sess.NextToolResult(coords, "web_search", map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("NextToolResult: %v", err)
	}
	if res["results"] == nil {
		t.Errorf("result = %+v, want the recorded results", res)
	}

	_, err = sess.NextToolResult(coords, "web_search", nil)
	me, ok := err.(*MissError)
	if !ok || me.Class != ClassExtra {
		t.Fatalf("second call: err = %v, want an extra MissError", err)
	}
}

func TestUserTurn(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0().Add(time.Second), "node-b", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"text":"later message"}]}]`,
		}),
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"system","parts":[{"text":"sys"}]},{"role":"user","parts":[{"text":"do the task"}]}]`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := sess.UserTurn()
	if !ok {
		t.Fatalf("UserTurn: not found")
	}
	if got != "do the task" {
		t.Errorf("UserTurn = %q, want %q (earliest stream's user message)", got, "do the task")
	}
}

func TestLoad_Zip(t *testing.T) {
	entries := []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"hi"}]}`,
		}),
	}
	path := writeZip(t, entries, Manifest{QuackVersion: "v-test", SessionID: "chat-1"})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.manifest.SessionID != "chat-1" {
		t.Errorf("manifest.SessionID = %q, want chat-1", sess.manifest.SessionID)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	if _, err := sess.NextChat(coords, "worker-model", nil); err != nil {
		t.Fatalf("NextChat: %v", err)
	}
}

// TestContentHash pins the drift hash algorithm's stability against
// inference/emit.go's contentHash - the two must compute the SAME digest
// over the same bytes for drift comparison to mean anything.
func TestContentHash(t *testing.T) {
	b := []byte(`{"role":"system","parts":[{"text":"hello"}]}`)
	sum := sha256.Sum256(b)
	want := hex.EncodeToString(sum[:])[:16]
	if got := contentHash(b); got != want {
		t.Errorf("contentHash = %q, want %q", got, want)
	}
}
