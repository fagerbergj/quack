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

// evalResult builds one gen_ai.evaluation.result entry - vetting/judge.go's
// emitEvaluationResults, no gen_ai.operation.name (see Session.ingest).
func evalResult(ts time.Time, node, round, responseID, criterion string, score float64) entry {
	return entry{ts: ts, attrs: map[string]any{
		"gen_ai.response.id":            responseID,
		"gen_ai.evaluation.name":        criterion,
		"gen_ai.evaluation.score.value": score,
		"gen_ai.evaluation.explanation": "because",
		"quack.node":                    node,
		"quack.round":                   round,
	}}
}

// rootChat builds a root-stream (top-level orchestrator) chat entry - the
// zero-value Node/Agent/Round every orchestrator-level call carries, since
// ledger.WithCoords is only ever called from a node's worker/judge round.
func rootChat(ts time.Time, model string, extra map[string]any) entry {
	return chat(ts, "", "", "", model, extra)
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

// invokeAgent builds one hand-crafted "invoke_agent" ledger line - mirrors
// chat/execTool above.
func invokeAgent(ts time.Time, node, agent, round string, sent, received []string) entry {
	toRaw := func(msgs []string) []json.RawMessage {
		out := make([]json.RawMessage, len(msgs))
		for i, m := range msgs {
			out[i] = json.RawMessage(m)
		}
		return out
	}
	sentJSON, _ := json.Marshal(toRaw(sent))
	receivedJSON, _ := json.Marshal(toRaw(received))
	return entry{ts: ts, attrs: map[string]any{
		"gen_ai.operation.name":  "invoke_agent",
		"gen_ai.agent.name":      agent,
		"gen_ai.input.messages":  string(sentJSON),
		"gen_ai.output.messages": string(receivedJSON),
		"quack.node":             node,
		"quack.round":            round,
	}}
}

func TestNextInvokeAgent_HappyPathAndExtra(t *testing.T) {
	path := writeJSONL(t, []entry{
		invokeAgent(t0(), "node-a", "code-implementer", "worker-r0",
			[]string{`{"id":1,"method":"initialize"}`},
			[]string{`{"id":1,"result":{}}`, `{"method":"session/update"}`}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "code-implementer", Round: "worker-r0"}
	sent, received, err := sess.NextInvokeAgent(coords, "code-implementer")
	if err != nil {
		t.Fatalf("NextInvokeAgent: %v", err)
	}
	if len(sent) != 1 || len(received) != 2 {
		t.Fatalf("sent=%d received=%d, want 1/2", len(sent), len(received))
	}

	_, _, err = sess.NextInvokeAgent(coords, "code-implementer")
	me, ok := err.(*MissError)
	if !ok || me.Class != ClassExtra {
		t.Fatalf("second call: err = %v, want an extra MissError", err)
	}
}

func TestNextInvokeAgent_Mismatched(t *testing.T) {
	path := writeJSONL(t, []entry{
		invokeAgent(t0(), "node-a", "code-implementer", "worker-r0", nil, nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	coords := ledger.Coords{Node: "node-a", Agent: "code-implementer", Round: "worker-r0"}
	_, _, err = sess.NextInvokeAgent(coords, "code-reviewer")
	me, ok := err.(*MissError)
	if !ok {
		t.Fatalf("err type = %T, want *MissError", err)
	}
	if me.Class != ClassMismatched {
		t.Errorf("Class = %q, want mismatched", me.Class)
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

// TestUserTurns_MultiTurn: a 2-turn conversation - each root-stream chat call
// carries the full history so far - returns both turns, oldest first, and
// does not repeat turn 1 just because it reappears in turn 2's context.
// Node-level (non-root) chat calls, which carry a role:user task prompt of
// their own, must NOT be picked up as an end-user turn.
func TestUserTurns_MultiTurn(t *testing.T) {
	path := writeJSONL(t, []entry{
		rootChat(t0(), "orch-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"text":"turn one"}]}]`,
		}),
		chat(t0().Add(time.Second), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"text":"do subtask X"}]}]`,
		}),
		rootChat(t0().Add(2*time.Second), "orch-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"text":"turn one"}]},{"role":"model","parts":[{"text":"ok"}]},{"role":"user","parts":[{"text":"turn two"}]}]`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := sess.UserTurns()
	want := []string{"turn one", "turn two"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("UserTurns = %v, want %v", got, want)
	}
}

func TestUserTurns_NoRootStream(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"text":"task prompt"}]}]`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := sess.UserTurns(); got != nil {
		t.Errorf("UserTurns = %v, want nil (no root-stream call recorded)", got)
	}
}

func TestFinalAnswer(t *testing.T) {
	path := writeJSONL(t, []entry{
		rootChat(t0(), "orch-model", map[string]any{
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"first reply"}]}`,
		}),
		rootChat(t0().Add(time.Second), "orch-model", map[string]any{
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"final reply"}]}`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := sess.FinalAnswer()
	if !ok {
		t.Fatalf("FinalAnswer: not found")
	}
	if got != "final reply" {
		t.Errorf("FinalAnswer = %q, want %q", got, "final reply")
	}
}

// TestEvaluationResults: evaluation.result events carry no
// gen_ai.operation.name and no stream identity replay matches on - they must
// come back from EvaluationResults regardless, oldest first, without
// polluting Report()'s stream accounting.
func TestEvaluationResults(t *testing.T) {
	path := writeJSONL(t, []entry{
		evalResult(t0().Add(time.Second), "node-a", "judge-r1", "judge-r1", "accuracy", 0.9),
		evalResult(t0(), "node-a", "judge-r1", "judge-r1", "clarity", 0.6),
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := sess.EvaluationResults()
	if len(got) != 2 {
		t.Fatalf("EvaluationResults len = %d, want 2", len(got))
	}
	if got[0].Criterion != "clarity" || got[0].Score != 0.6 {
		t.Errorf("got[0] = %+v, want clarity/0.6 (oldest first)", got[0])
	}
	if got[1].Criterion != "accuracy" || got[1].Score != 0.9 {
		t.Errorf("got[1] = %+v, want accuracy/0.9", got[1])
	}
	// An evaluation.result event carries no chat/tool/agent identity - it must
	// not show up as a stream in Report().
	rep := sess.Report()
	if len(rep.Streams) != 1 {
		t.Errorf("Report().Streams = %+v, want exactly the one worker chat stream", rep.Streams)
	}
}
