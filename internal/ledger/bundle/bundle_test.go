package bundle

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

// toEntries pushes hand-built attribute maps through the real Exporter, so
// these fixtures are shaped exactly as production records them.
func toEntries(t *testing.T, entries []entry) []ledger.Entry {
	t.Helper()
	store := ledgertest.NewMemStore()
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	lg := lp.Logger("test")
	for _, e := range entries {
		var rec otellog.Record
		rec.SetTimestamp(e.ts)
		rec.AddAttributes(attribute.String("gen_ai.conversation.id", "chat-1"))
		for k, v := range e.attrs {
			switch x := v.(type) {
			case string:
				rec.AddAttributes(attribute.String(k, x))
			case float64:
				rec.AddAttributes(attribute.Float64(k, x))
			case int:
				rec.AddAttributes(attribute.Int64(k, int64(x)))
			case []any:
				vals := make([]attribute.Value, len(x))
				for i, sv := range x {
					vals[i] = attribute.StringValue(sv.(string))
				}
				rec.AddAttributes(attribute.Slice(k, vals...))
			default:
				t.Fatalf("unsupported attr type %T for %s", v, k)
			}
		}
		lg.Emit(context.Background(), rec)
	}
	out, err := store.ReadEntries(context.Background(), "chat-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// entry is a small builder for one hand-crafted ledger line, so these tests
// exercise Session directly without going through the full emission path.
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

// invokeAgent builds one hand-crafted "invoke_agent" ledger line - mirrors chat above.
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

func writeJSONL(t *testing.T, entries []entry) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entries.jsonl")
	var sb strings.Builder
	for _, e := range toEntries(t, entries) {
		b, err := json.Marshal(e)
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

func writeZip(t *testing.T, entries []entry, manifest ledger.Manifest) string {
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
	for _, e := range toEntries(t, entries) {
		b, err := json.Marshal(e)
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

func TestLoad_Zip(t *testing.T) {
	entries := []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"do the task"}]}]`,
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"hi"}]}`,
		}),
	}
	path := writeZip(t, entries, ledger.Manifest{QuackVersion: "v-test", LedgerVersion: ledger.LedgerVersion, SessionID: "chat-1"})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.manifest.SessionID != "chat-1" {
		t.Errorf("manifest.SessionID = %q, want chat-1", sess.manifest.SessionID)
	}
	runs := sess.NodeRuns(map[string]bool{"worker": true})
	if len(runs) != 1 {
		t.Fatalf("NodeRuns from a zip-loaded bundle = %d, want 1", len(runs))
	}
}

// TestUserTurns_MultiTurn: each root-stream chat call carries the full history so far -
// return both turns, oldest first, not repeating turn 1 from turn 2's context; node-level
// (non-root) role:user task prompts must NOT be picked up as end-user turns.
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

// TestEvaluationResults: evaluation.result events carry no gen_ai.operation.name
// and no stream identity - they must come back oldest-first.
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
}

// TestNodeRuns_ACPOnlyStream: an ACP-backed agent (code-reviewer) emits no
// llm.call, only one invoke_agent record per round - NodeRuns must still
// produce a run for it, not drop it.
func TestNodeRuns_ACPOnlyStream(t *testing.T) {
	sent := []string{`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"review this diff"}]}}`}
	received := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Looks "}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"good."}}}}`,
	}
	path := writeJSONL(t, []entry{
		invokeAgent(t0(), "node-a", "code-reviewer", "worker-r0", sent, received),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	runs := sess.NodeRuns(map[string]bool{"code-reviewer": true})
	if len(runs) != 1 {
		t.Fatalf("NodeRuns len = %d, want 1 (ACP-only stream must not be dropped)", len(runs))
	}
	for _, run := range runs {
		if run.Task != "review this diff" {
			t.Errorf("Task = %q, want the sent session/prompt text", run.Task)
		}
		if run.Answer != "Looks good." {
			t.Errorf("Answer = %q, want the concatenated agent_message_chunk text", run.Answer)
		}
	}
}

// TestNodeRuns_ACPAnswerResetsOnToolCall: the delivered answer only ever
// contains text after the last tool call (translate.go resets t.answer on
// each u.ToolCall) - NodeRuns must mirror that, not concatenate the whole round.
func TestNodeRuns_ACPAnswerResetsOnToolCall(t *testing.T) {
	sent := []string{`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"review this diff"}]}}`}
	received := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Let me look at the files first."}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t1"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Final review: looks good."}}}}`,
	}
	path := writeJSONL(t, []entry{
		invokeAgent(t0(), "node-a", "code-reviewer", "worker-r0", sent, received),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	runs := sess.NodeRuns(map[string]bool{"code-reviewer": true})
	if len(runs) != 1 {
		t.Fatalf("NodeRuns len = %d, want 1", len(runs))
	}
	for _, run := range runs {
		if run.Answer != "Final review: looks good." {
			t.Errorf("Answer = %q, want only the text after the last tool call", run.Answer)
		}
	}
}

// TestNodeRuns_DraftPlusRevise: draft and revise are separate rounds/streams
// of the same node - NodeRuns must collapse them into one run, task from the
// draft, answer from the later (by At) revise.
func TestNodeRuns_DraftPlusRevise(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "synthesizer", "worker-r0", "gpt", map[string]any{
			"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"summarize this"}]}]`,
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"draft answer"}]}`,
		}),
		chat(t0().Add(time.Minute), "node-a", "synthesizer", "worker-r1", "gpt", map[string]any{
			"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"judge feedback + prior answer inlined"}]}]`,
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"revised answer"}]}`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	runs := sess.NodeRuns(map[string]bool{"synthesizer": true})
	if len(runs) != 1 {
		t.Fatalf("NodeRuns len = %d, want 1 (draft+revise must collapse to one run)", len(runs))
	}
	for _, run := range runs {
		if run.Task != "summarize this" {
			t.Errorf("Task = %q, want the draft round's task, never the synthetic revise prompt", run.Task)
		}
		if run.Answer != "revised answer" {
			t.Errorf("Answer = %q, want the revise round's (latest) answer", run.Answer)
		}
	}
}
