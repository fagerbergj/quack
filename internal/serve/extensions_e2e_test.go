package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// directAnswerModel is a minimal model.LLM that answers with plain text and
// no tool calls, so the orchestrator's top-level llmagent completes without
// touching plan/execute - the same shape as rest.stubModel, reused here
// because the dispatch loop only needs to prove it reached a real Answer.
type directAnswerModel struct{}

func (directAnswerModel) Name() string { return "direct-answer-stub" }

func (directAnswerModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "quack quack"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// newExtTestStack builds a real (sqlite) store + a stub-model orchestrator -
// enough to drive a full dispatch without a live LLM - and stores it into
// orchRef the same way buildFromConfig does once the orchestrator exists.
// The returned TurnAwareService is a row-backed artifact service sharing
// the same store, exactly like buildFromConfig wires attachments in production.
func newExtTestStack(t *testing.T) (*store.Store, *orchestrator.Orchestrator, *stream.Hub, *store.TurnAwareService, *workspace.Jail) {
	t.Helper()
	return newExtTestStackWithModel(t, directAnswerModel{})
}

// newExtTestStackWithModel is newExtTestStack with the orchestrator's model
// swapped out - used by tests that need a specific failure mode from the
// model itself (e.g. #1156's gateway-error-during-planning repro), rather
// than the always-succeeds directAnswerModel.
func newExtTestStackWithModel(t *testing.T, m model.LLM) (*store.Store, *orchestrator.Orchestrator, *stream.Hub, *store.TurnAwareService, *workspace.Jail) {
	t.Helper()
	return newExtTestStackWithModelAndAgents(t, m, nil)
}

// newExtTestStackWithModelAndAgents is newExtTestStackWithModel with the
// planner's known-agent roster also swappable - needed by any test whose
// scripted plan names a real agent (e.g. dag's own reviewerAgent), since
// dag.Planner.Build rejects an unknown agent name before anything else runs.
func newExtTestStackWithModelAndAgents(t *testing.T, m model.LLM, agents []dag.AgentInfo) (*store.Store, *orchestrator.Orchestrator, *stream.Hub, *store.TurnAwareService, *workspace.Jail) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	artifactSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifactSvc)
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, map[string]model.LLM{}, nil,
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	planner := dag.NewPlanner(agents, nil, nil)
	orch := orchestrator.New(st.Sessions, m, "You are a test duck.", planner, ex, nil, nil, nil)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}
	return st, orch, stream.NewHub(), store.NewTurnAwareService(artifactSvc), jail
}

// noopModulesConfig parses an extensions: block through the REAL inline-map
// path (config.ExtensionsConfig's yaml tag), so the test exercises the same
// opaque-node shape production config.Load produces.
func noopModulesConfig(t *testing.T, workspaceRoot string, yamlBlock string) *config.Config {
	t.Helper()
	var ext config.ExtensionsConfig
	if err := yaml.Unmarshal([]byte(yamlBlock), &ext); err != nil {
		t.Fatalf("parse extensions block: %v", err)
	}
	return &config.Config{Extensions: ext, Workspace: config.WorkspaceConfig{Root: workspaceRoot}}
}

// waitRunSettled blocks until chatID's background run goroutine has stamped
// a terminal outcome - the last durable write driveExtensionRun makes before
// returning. Callers use this to fully drain one dispatch's goroutine before
// firing another, so two runs' event-log writes never race each other (and
// leak past their test into whichever runs next in the same process).
func waitRunSettled(t *testing.T, st *store.Store, chatID string) {
	t.Helper()
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := st.GetChat(context.Background(), chatID)
		return c != nil && c.RunStatus != ""
	})
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSDKExtensionDispatchLoop is the spec's phase-2 loop-proof test: it
// configures noop, dispatches over real HTTP against a stub-model
// orchestrator, and asserts the run lands as a normal chat+turn and that
// /status only advances once RunEnded actually fires.
func TestSDKExtensionDispatchLoop(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "noop:\n  greeting: e2e\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 1 || sdkExts[0].name != "noop" {
		t.Fatalf("sdkExts = %+v, want one noop extension", sdkExts)
	}

	h := server.New(server.Options{
		REST:          &rest.Handler{},
		SDKExtensions: sdkExtensionMounts(sdkExts),
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/noop/dispatch", "text/plain", strings.NewReader("hello from the e2e test"))
	if err != nil {
		t.Fatalf("POST /noop/dispatch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("dispatch status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	// (b) /status only counts a run once RunEnded fires - proof the whole
	// register -> route -> dispatch -> run -> RunEnded loop completed, not
	// just that the HTTP call was accepted.
	getStatus := func() map[string]any {
		r, err := http.Get(ts.URL + "/noop/status")
		if err != nil {
			t.Fatalf("GET /status: %v", err)
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode status: %v (body=%s)", err, body)
		}
		return out
	}
	waitUntil(t, 5*time.Second, func() bool {
		return getStatus()["dispatches"] == float64(1)
	})

	// (a) the run appears as a normal chat with a turn, namespaced as
	// ext:<extension>:<LocalID> so it can never collide with another
	// extension's or a user's chat.
	ctx := context.Background()
	chats, _, err := st.ListChats(ctx, 10, "", store.ChatsScope{Active: true})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	var chatID string
	for _, c := range chats {
		if strings.HasPrefix(c.ID, "ext:noop:noop-") {
			chatID = c.ID
		}
	}
	if chatID == "" {
		t.Fatalf("no ext:noop:noop-* chat found among %d chats", len(chats))
	}
	turns, err := st.ListTurns(ctx, chatID)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns for %s = %d, want 1", chatID, len(turns))
	}
	c, err := st.GetChat(ctx, chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat(%s) = %v, %v", chatID, c, err)
	}
	if c.Title != "noop test" {
		t.Errorf("chat title = %q, want origin label %q", c.Title, "noop test")
	}
}

// (c) An unconfigured noop registers no routes at all - 404, not a
// dormant-but-mounted handler.
func TestSDKExtensionUnconfiguredExtensionRegistersNoRoutes(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := &config.Config{Workspace: config.WorkspaceConfig{Root: t.TempDir()}}
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 0 {
		t.Fatalf("sdkExts = %+v, want none (vanilla config)", sdkExts)
	}

	h := server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(sdkExts)})
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/noop/status")
	if err != nil {
		t.Fatalf("GET /noop/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d (extension never mounted)", resp.StatusCode, http.StatusNotFound)
	}
}

// (d) extensions: naming an extension that isn't compiled in fails startup loudly.
func TestSDKExtensionUnknownNameFailsStartup(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "bogus-extension:\n  key: value\n")
	_, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an unconfigured/uncompiled extension name")
	}
	if !strings.Contains(err.Error(), "bogus-extension") {
		t.Errorf("err = %v, want it to name the offending extension", err)
	}
}

// enabled: false leaves a configured-and-compiled extension dormant, exactly
// like an absent block - no construction, no routes.
func TestSDKExtensionDisabledStaysDormant(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "noop:\n  enabled: false\n  greeting: e2e\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 0 {
		t.Fatalf("sdkExts = %+v, want none (enabled: false)", sdkExts)
	}

	h := server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(sdkExts)})
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/noop/status")
	if err != nil {
		t.Fatalf("GET /noop/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d (disabled extension never mounted)", resp.StatusCode, http.StatusNotFound)
	}
}

// data_dir overrides Host.DataDir's default; the default (<workspace>/extensions/<name>)
// must never get created when an override is given.
func TestSDKExtensionDataDirOverrideUsed(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	workspaceRoot := t.TempDir()
	customDataDir := filepath.Join(t.TempDir(), "custom-noop-data")
	cfg := noopModulesConfig(t, workspaceRoot, "noop:\n  data_dir: "+customDataDir+"\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 1 {
		t.Fatalf("sdkExts = %+v, want one noop extension", sdkExts)
	}
	if _, err := os.Stat(customDataDir); err != nil {
		t.Errorf("data_dir override %s not created: %v", customDataDir, err)
	}
	defaultDataDir := filepath.Join(workspaceRoot, "extensions", "noop")
	if _, err := os.Stat(defaultDataDir); err == nil {
		t.Errorf("default data dir %s should not exist when data_dir is overridden", defaultDataDir)
	}
}

// The reserved base-config keys (enabled, data_dir) must not break an
// extension's own yaml.Unmarshal of the same raw bytes - noop only knows
// "greeting", and yaml tolerates unknown fields by default.
func TestSDKExtensionReservedKeysToleratedByExtensionConfig(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "noop:\n  enabled: true\n  data_dir: \"\"\n  greeting: still works\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 1 || sdkExts[0].name != "noop" {
		t.Fatalf("sdkExts = %+v, want one noop extension", sdkExts)
	}
}

// The registry is process-global and Register panics on a duplicate, so this
// registers once per process (not per test run) to stay safe under -count>1.
var reservedNameFactoryCalled atomic.Bool

func init() {
	extsdk.Register("chat", func(extsdk.Host, []byte) (extsdk.Extension, error) {
		reservedNameFactoryCalled.Store(true)
		return nil, errors.New("factory must never be called for a reserved-name collision")
	})
}

// A registered+configured extension whose name collides with a reserved
// route fails startup loudly, naming both the extension and the collision.
func TestSDKExtensionReservedNameCollisionFailsStartup(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "chat:\n  key: value\n")
	_, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an extension name colliding with a reserved route")
	}
	if !strings.Contains(err.Error(), "chat") {
		t.Errorf("err = %v, want it to name the extension", err)
	}
	if reservedNameFactoryCalled.Load() {
		t.Error("factory was called for a reserved-name collision; startup must fail before construction")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("err = %v, want it to name the reserved-route collision", err)
	}
}

// (e) dispatching twice to the same LocalID appends a second turn to the
// same chat rather than starting a new one - exercised directly against the
// dispatch adapter (newExtDispatch) since noop's own dispatch policy always
// mints a fresh id, but quack's LocalID-reuse contract is what's under test.
func TestSDKExtensionRedispatchSameChatIDAppendsTurn(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)

	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	const localID = "redispatch-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "first turn"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// Drain the first run fully before firing the second - two overlapping
	// runs on the same chat would race each other's event-log writes.
	waitRunSettled(t, st, chatID)

	req.Ask.Message = "second turn"
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	ctx := context.Background()
	turns, err := st.ListTurns(ctx, chatID)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns for %s = %d, want 2", chatID, len(turns))
	}
	chats, _, err := st.ListChats(ctx, 10, "", store.ChatsScope{Active: true})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	count := 0
	for _, c := range chats {
		if c.ID == chatID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("chat rows for %s = %d, want exactly 1 (re-dispatch must append, not duplicate)", chatID, count)
	}
}

// (f) an unknown Workflow name fails the dispatch call itself, before any
// chat row is created - never a silent hint the planner might ignore.
func TestSDKExtensionUnknownWorkflowErrorsCreatesNoChat(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)

	var extHolder atomic.Pointer[extsdk.Extension]
	shapes := []workflowcatalog.Shape{{Name: "document-ingest"}}
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, shapes, artifacts)

	const localID = "unknown-workflow-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{
		Chat: extsdk.ChatRef{LocalID: localID},
		Ask:  extsdk.Ask{Message: "hello"},
		Run:  extsdk.RunConfig{Workflow: "bogus-shape"},
	}
	if err := dispatch(context.Background(), req); err == nil {
		t.Fatal("expected an error for an unknown workflow name")
	} else if !strings.Contains(err.Error(), "bogus-shape") {
		t.Errorf("err = %v, want it to name the offending workflow", err)
	}

	ctx := context.Background()
	c, err := st.GetChat(ctx, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if c != nil {
		t.Fatalf("GetChat(%s) = %+v, want no chat created", chatID, c)
	}
}

// TestSDKExtensionDispatchRejectsWhileDraining proves the shutdown-drain
// gate (#888) covers the extension path, not just the REST handler tested
// by rest.TestSendChatMessage_DrainingRejects503.
func TestSDKExtensionDispatchRejectsWhileDraining(t *testing.T) {
	st, orch, hub, _, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)

	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, nil)

	hub.BeginDraining()
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "draining-fixture"}, Ask: extsdk.Ask{Message: "hello"}}
	if err := dispatch(context.Background(), req); err == nil {
		t.Fatal("expected an error while the hub is draining")
	} else if !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("err = %v, want it to explain the server is shutting down", err)
	}

	if c, _ := st.GetChat(context.Background(), "ext:noop:draining-fixture"); c != nil {
		t.Fatalf("GetChat = %+v, want no chat created for a rejected dispatch", c)
	}
}

// TestSDKExtensionDispatchPreservesTraceContinuity pins the design doc's OTel
// test case: Dispatch must preserve the caller's context so the extension's
// inbound span parents the whole run trace, not start a disconnected one.
func TestSDKExtensionDispatchPreservesTraceContinuity(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	ctx, inboundSpan := tp.Tracer("quack-ext-test").Start(context.Background(), "inbound")
	wantTraceID := inboundSpan.SpanContext().TraceID()
	inboundSpan.End()

	const localID = "trace-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(ctx, req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	found := false
	for _, s := range exp.GetSpans() {
		if s.Name != "quack.run" {
			continue
		}
		found = true
		if got := s.SpanContext.TraceID(); got != wantTraceID {
			t.Errorf("run span trace id = %s, want %s (caller's context should parent the run)", got, wantTraceID)
		}
	}
	if !found {
		t.Fatal("no quack.run span exported")
	}
}

// extAttachStub plays the orchestrator (routes on the "plan" tool's
// presence), the judge (submit_verdict), and the "media" worker - recording
// the bytes/mime the worker actually received off req.Contents. Mirrors
// rest.attachStub (internal/server/rest/attachments_test.go); duplicated
// rather than exported since it's package-local test fixture, not API.
type extAttachStub struct {
	mu          sync.Mutex
	workerCalls int
	seenBytes   []byte
	seenMime    string
}

func (*extAttachStub) Name() string { return "extAttachStub" }

func (s *extAttachStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case extAttachStubHasTool(req, "submit_verdict"):
			yield(extAttachStubCall("submit_verdict", map[string]any{"score": 0.95, "feedback": ""}), nil)
			return
		case !extAttachStubHasTool(req, "plan"): // no plan tool ⇒ the media worker
			s.mu.Lock()
			s.workerCalls++
			for _, c := range req.Contents {
				if c == nil {
					continue
				}
				for _, p := range c.Parts {
					if p != nil && p.InlineData != nil {
						s.seenBytes = append([]byte(nil), p.InlineData.Data...)
						s.seenMime = p.InlineData.MIMEType
					}
				}
			}
			s.mu.Unlock()
			yield(extAttachStubText("the image shows a duck"), nil)
			return
		}
		if id, ok := extAttachStubPlanID(req); ok {
			yield(extAttachStubCall("execute", map[string]any{"plan_id": id}), nil)
			return
		}
		yield(extAttachStubCall("plan", map[string]any{"nodes": []any{map[string]any{
			"id": "n1", "agent": "media", "task": "describe the attached image", "depends_on": []any{},
		}}}), nil)
	}
}

func extAttachStubHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func extAttachStubPlanID(req *model.LLMRequest) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "plan" {
				continue
			}
			if id, ok := p.FunctionResponse.Response["plan_id"].(string); ok && id != "" {
				return id, true
			}
		}
	}
	return "", false
}

func extAttachStubText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop, TurnComplete: true,
	}
}

func extAttachStubCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}},
		FinishReason: genai.FinishReasonStop, TurnComplete: true,
	}
}

// newExtAttachmentTestStack is newExtTestStack, but with a one-node "media"
// DAG behind the orchestrator, so a dispatched attachment has somewhere to
// hydrate to - newExtTestStack's planner has no agents at all.
func newExtAttachmentTestStack(t *testing.T) (*store.Store, *orchestrator.Orchestrator, *stream.Hub, *store.TurnAwareService, *extAttachStub) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	artifactSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifactSvc)
	artifacts := store.NewTurnAwareService(artifactSvc)

	stub := &extAttachStub{}
	// Only the media worker's model is wrapped with hydration - mirrors
	// production (inference.NewModel wraps every real model this way).
	hydratedStub := inference.HydratingModelForTesting(stub, artifacts)
	worker, err := llmagent.New(llmagent.Config{
		Name: "media", Model: hydratedStub, Description: "reads images", Instruction: "ROLE:media Describe the attached image.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{"media": worker}, map[string]model.LLM{"media": hydratedStub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.5, JudgeRounds: 1} },
		map[string]bool{"media": true})
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "media", Description: "reads images"}}, nil, nil)
	orch := orchestrator.New(st.Sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
	return st, orch, stream.NewHub(), artifacts, stub
}

// extFakePNG is not a real PNG - the stub model never decodes it - but is
// distinctive enough to prove byte-for-byte round-trip and to search the
// persisted plan/session JSON for.
var extFakePNG = []byte("\x89PNG-ext-fake-pixel-data-0123456789abcdef")

// TestSDKExtensionDispatch_AttachmentHydratesAndPersistsReferenceOnly is the
// extension-dispatch mirror of rest.TestAttachmentRoundTrip: an attachment
// on Ask.Attachments reaches the media worker's model as real bytes, while
// the persisted DAG plan and ADK session events carry only a
// quack-artifact:// reference - never the bytes themselves.
func TestSDKExtensionDispatch_AttachmentHydratesAndPersistsReferenceOnly(t *testing.T) {
	st, orch, hub, artifacts, stub := newExtAttachmentTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	const localID = "attachment-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{
		Chat: extsdk.ChatRef{LocalID: localID},
		Ask: extsdk.Ask{
			Message:     "what is in this image?",
			Attachments: []extsdk.Attachment{{Name: "photo.png", MIME: "image/png", Data: extFakePNG}},
		},
	}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	stub.mu.Lock()
	gotBytes, gotMime, calls := stub.seenBytes, stub.seenMime, stub.workerCalls
	stub.mu.Unlock()
	if calls == 0 {
		t.Fatal("the media worker was never invoked")
	}
	if !bytes.Equal(gotBytes, extFakePNG) {
		t.Errorf("worker saw bytes %q, want the original attachment %q", gotBytes, extFakePNG)
	}
	if gotMime != "image/png" {
		t.Errorf("worker saw mime %q, want image/png", gotMime)
	}

	ctx := context.Background()
	dp, err := st.GetLatestDagPlan(ctx, chatID)
	if err != nil || dp == nil {
		t.Fatalf("GetLatestDagPlan: %v", err)
	}
	if bytes.Contains([]byte(dp.PlanJSON), extFakePNG) {
		t.Errorf("persisted DAG plan JSON contains the raw attachment bytes:\n%s", dp.PlanJSON)
	}
	if bytes.Contains([]byte(dp.PlanJSON), []byte(base64.StdEncoding.EncodeToString(extFakePNG))) {
		t.Errorf("persisted DAG plan JSON contains the base64-encoded attachment bytes:\n%s", dp.PlanJSON)
	}

	resp, err := st.Sessions.Get(ctx, &session.GetRequest{AppName: orchestrator.AppName, UserID: extRunUserID, SessionID: chatID})
	if err != nil {
		t.Fatalf("Sessions.Get: %v", err)
	}
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.InlineData != nil && bytes.Equal(p.InlineData.Data, extFakePNG) {
				t.Fatalf("a session event (author=%s) carries the raw attachment bytes", ev.Author)
			}
		}
	}
}

// planToolProbeModel stands in for the orchestrator's own top-level model,
// recording whether any call it received offered the "plan" tool - the
// actual mechanism of "planning" in this codebase. It may still legitimately
// be called for the unrelated post-execution format pass (finalizeAnswer ->
// formatAnswer), which never offers tools at all; only a call that CAN
// decompose into nodes counts as the planner LLM call this proves is
// skipped.
type planToolProbeModel struct{ sawPlanTool atomic.Bool }

func (*planToolProbeModel) Name() string { return "plan-tool-probe-stub" }

func (m *planToolProbeModel) SawPlanTool() bool { return m.sawPlanTool.Load() }

func (m *planToolProbeModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	if extAttachStubHasTool(req, "plan") {
		m.sawPlanTool.Store(true)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestSDKExtensionDispatch_BoundWorkflowSkipsPlannerLLM is test case 4
// (workflow binding): a dispatch naming a shaped catalog entry
// runs the bound node straight through the graph executor - the
// orchestrator's own LLM (the planner call) is never invoked - and the
// persisted plan carries the node's task with {{ask}} substituted.
func TestSDKExtensionDispatch_BoundWorkflowSkipsPlannerLLM(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	artifactSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifactSvc)
	artifacts := store.NewTurnAwareService(artifactSvc)

	workerStub := &extAttachStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "worker", Model: workerStub, Description: "does work", Instruction: "ROLE:worker Do the task.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{"worker": worker}, map[string]model.LLM{"worker": workerStub},
		vetting.NewJudgeFactory(workerStub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.5, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "worker", Description: "does work"}}, nil, nil)
	orchModel := &planToolProbeModel{}
	orch := orchestrator.New(st.Sessions, orchModel, "You are the orchestrator.", planner, ex, nil, nil, nil)

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	hub := stream.NewHub()
	var extHolder atomic.Pointer[extsdk.Extension]

	shapes := []workflowcatalog.Shape{{
		Name: "document-ingest",
		Nodes: []config.WorkflowNode{
			{ID: "n1", Agent: "worker", Task: "process: {{ask}}"},
		},
	}}
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, shapes, artifacts)

	const localID = "bound-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{
		Chat: extsdk.ChatRef{LocalID: localID},
		Ask:  extsdk.Ask{Message: "scan-0042.pdf"},
		Run:  extsdk.RunConfig{Workflow: "document-ingest"},
	}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	if orchModel.SawPlanTool() {
		t.Error("orchestrator's own LLM saw the plan tool; a bound workflow dispatch must skip the planner LLM call entirely")
	}
	workerStub.mu.Lock()
	workerCalls := workerStub.workerCalls
	workerStub.mu.Unlock()
	if workerCalls == 0 {
		t.Error("the bound node's own worker agent was never invoked")
	}

	ctx := context.Background()
	dp, err := st.GetLatestDagPlan(ctx, chatID)
	if err != nil || dp == nil {
		t.Fatalf("GetLatestDagPlan: %v, %v", dp, err)
	}
	if !strings.Contains(dp.PlanJSON, "process: scan-0042.pdf") {
		t.Errorf("persisted plan JSON = %s, want the bound node's task with {{ask}} substituted", dp.PlanJSON)
	}
}

// hintProbeModel answers differently depending on whether the orchestrator's
// LLM turn actually saw the workflow-hint text composeDispatchMessage folds
// in - proof the unshaped path still reaches the planner, hint and all.
type hintProbeModel struct{ hint string }

func (hintProbeModel) Name() string { return "hint-probe-stub" }

func (m hintProbeModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		found := false
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && strings.Contains(p.Text, m.hint) {
					found = true
				}
			}
		}
		answer := "no hint"
		if found {
			answer = "hint received"
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: answer}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestSDKExtensionDispatch_UnshapedWorkflowFoldsHintIntoMessage is test case
// 2 (workflow binding): a dispatch naming a catalog entry with
// no bound Nodes still runs the orchestrator's own LLM turn, with the
// workflow named as a hint in the composed message - unchanged behavior for
// every shape that predates binding.
func TestSDKExtensionDispatch_UnshapedWorkflowFoldsHintIntoMessage(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	artifactSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifactSvc)
	artifacts := store.NewTurnAwareService(artifactSvc)

	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, map[string]model.LLM{}, nil,
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	orch := orchestrator.New(st.Sessions, hintProbeModel{hint: `Use the "unshaped-hint" workflow shape`}, "You are a test duck.", planner, ex, nil, nil, nil)

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	hub := stream.NewHub()
	var extHolder atomic.Pointer[extsdk.Extension]

	// No Nodes: this shape stays a planner hint, never a binding.
	shapes := []workflowcatalog.Shape{{Name: "unshaped-hint", Trigger: "t", DAGShape: "s"}}
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, shapes, artifacts)

	const localID = "unshaped-fixture"
	const chatID = "ext:noop:" + localID
	req := extsdk.DispatchRequest{
		Chat: extsdk.ChatRef{LocalID: localID},
		Ask:  extsdk.Ask{Message: "hello"},
		Run:  extsdk.RunConfig{Workflow: "unshaped-hint"},
	}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	answer := orch.LatestAnswer(context.Background(), extRunUserID, chatID)
	if answer != "hint received" {
		t.Errorf("answer = %q, want %q (the orchestrator LLM must have seen the workflow hint)", answer, "hint received")
	}
}

// TestSDKExtensionUpdateChatOriginRefreshesBadge is #844's proof: a chat
// dispatched with badge "open" reads back "open" over the real GET
// /api/v1/chats/{id} route, Host.UpdateChatOrigin flips it, and the same GET
// shows the new badge - without a second Dispatch/run.
func TestSDKExtensionUpdateChatOriginRefreshesBadge(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)
	updateOrigin := newExtUpdateChatOrigin("noop", st, nil, nil)

	const localID = "badge-fixture"
	const chatID = "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#7", Kind: "issues", Badge: "open"}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	restHandler := rest.NewHandler(st, orch, nil, jail, hub, nil, "", nil, nil, artifacts, nil)
	ts := httptest.NewServer(server.New(server.Options{REST: restHandler}))
	defer ts.Close()

	getBadge := func() string {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/v1/chats/" + chatID)
		if err != nil {
			t.Fatalf("GET chat: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET chat status = %d, body = %s", resp.StatusCode, body)
		}
		var detail schema.ChatDetail
		if err := json.Unmarshal(body, &detail); err != nil {
			t.Fatalf("decode chat: %v (body=%s)", err, body)
		}
		if detail.Origin == nil || detail.Origin.Badge == nil {
			t.Fatalf("chat origin/badge missing: %+v", detail.Origin)
		}
		return *detail.Origin.Badge
	}

	if got := getBadge(); got != "open" {
		t.Fatalf("badge before update = %q, want open", got)
	}

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#7", Kind: "issues", Badge: "closed"}); err != nil {
		t.Fatalf("UpdateChatOrigin: %v", err)
	}

	if got := getBadge(); got != "closed" {
		t.Fatalf("badge after update = %q, want closed", got)
	}
}

// TestSDKExtensionUpdateChatOriginUnknownChatErrors pins the no-op contract
// on the quack side: a localID that never reached Dispatch reports
// extsdk.ErrUnknownChat, never a silently-created bare chat row.
func TestSDKExtensionUpdateChatOriginUnknownChatErrors(t *testing.T) {
	st, _, _, _, _ := newExtTestStack(t)
	updateOrigin := newExtUpdateChatOrigin("noop", st, nil, nil)

	err := updateOrigin("never-dispatched", extsdk.ChatOrigin{Extension: "noop", Label: "x", Badge: "closed"})
	if !errors.Is(err, extsdk.ErrUnknownChat) {
		t.Fatalf("err = %v, want ErrUnknownChat", err)
	}
	c, gerr := st.GetChat(context.Background(), "ext:noop:never-dispatched")
	if gerr != nil {
		t.Fatalf("GetChat: %v", gerr)
	}
	if c != nil {
		t.Fatalf("GetChat = %+v, want no chat row created by a failed update", c)
	}
}
