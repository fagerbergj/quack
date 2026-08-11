package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

// extRunUserID distinguishes extension-dispatched sessions from local UI and
// GitHub-driven ones, mirroring internal/github's runUserID convention.
const extRunUserID = "ext"

// builtSDKExtension pairs a constructed quack-extensions SDK module with the
// registration name its config and log lines are keyed by.
type builtSDKExtension struct {
	name string
	ext  extsdk.Extension
}

// buildSDKExtensions constructs every configured module named under
// extensions: that is also compiled in (sdk.Registered(), populated by
// extensions_registry.go's blank imports). A configured name absent from the
// registry, or one that fails ValidateExtensionName, fails startup loudly; a
// registered module absent from config, or configured with enabled: false,
// stays dormant - never constructed (design doc "Model"). orchRef is read
// lazily by the returned extensions' Dispatch closures: it isn't resolved
// until the caller Stores the orchestrator, built later in buildFromConfig.
func buildSDKExtensions(cfg *config.Config, st *store.Store, hub *stream.Hub, orchRef *atomic.Pointer[orchestrator.Orchestrator], artifacts *store.TurnAwareService) ([]builtSDKExtension, error) {
	factories := extsdk.Registered()
	names := make([]string, 0, len(cfg.Extensions.Modules))
	for name := range cfg.Extensions.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

	shapes := workflowcatalog.FromConfig(cfg.Skills.Workflows, cfg.Revision)

	built := make([]builtSDKExtension, 0, len(names))
	for _, name := range names {
		factory, ok := factories[name]
		if !ok {
			known := make([]string, 0, len(factories))
			for k := range factories {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("config: extensions.%s is not a compiled extension (compiled: %s)", name, strings.Join(known, ", "))
		}
		// Only a name that will actually be mounted needs to be route-safe -
		// a compiled-but-unconfigured module never reaches this check.
		if err := server.ValidateExtensionName(name); err != nil {
			return nil, fmt.Errorf("config: extensions.%s: %w", name, err)
		}
		node := cfg.Extensions.Modules[name]
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("extensions.%s: re-marshal config: %w", name, err)
		}

		var base extsdk.BaseConfig
		if err := yaml.Unmarshal(raw, &base); err != nil {
			return nil, fmt.Errorf("extensions.%s: parse base config: %w", name, err)
		}
		if base.Enabled != nil && !*base.Enabled {
			slog.Info("sdk extension disabled by config; staying dormant", "component", "startup", "extension", name)
			continue
		}

		dataDir := base.DataDir
		if dataDir == "" {
			dataDir = filepath.Join(cfg.Workspace.Root, "extensions", name)
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("extensions.%s: data dir: %w", name, err)
		}

		var extHolder atomic.Pointer[extsdk.Extension]
		host := extsdk.Host{
			Dispatch: newExtDispatch(name, orchRef, st, hub, &extHolder, shapes, artifacts),
			Log:      slog.Default().With("component", "ext."+name),
			DataDir:  dataDir,
		}
		ext, err := factory(host, raw)
		if err != nil {
			return nil, fmt.Errorf("extensions.%s: factory: %w", name, err)
		}
		extHolder.Store(&ext)
		built = append(built, builtSDKExtension{name: name, ext: ext})
		slog.Info("sdk extension enabled", "component", "startup", "extension", name)
	}
	return built, nil
}

// startSDKExtensions calls Start on every extension implementing
// sdk.Starter, failing loudly on the first error (design doc: "fail startup
// on error").
func startSDKExtensions(ctx context.Context, exts []builtSDKExtension) error {
	for _, e := range exts {
		starter, ok := e.ext.(extsdk.Starter)
		if !ok {
			continue
		}
		if err := starter.Start(ctx); err != nil {
			return fmt.Errorf("extensions.%s: start: %w", e.name, err)
		}
	}
	return nil
}

// stopSDKExtensions calls Stop on every extension implementing sdk.Stopper.
// Best-effort: Stop must already be idempotent per the SDK contract, so this
// runs unconditionally during shutdown even if Start never ran for some.
func stopSDKExtensions(ctx context.Context, exts []builtSDKExtension) {
	for _, e := range exts {
		stopper, ok := e.ext.(extsdk.Stopper)
		if !ok {
			continue
		}
		if err := stopper.Stop(ctx); err != nil {
			slog.Warn("extension stop failed", "component", "ext."+e.name, "err", err)
		}
	}
}

// sdkExtensionMounts adapts built extensions onto server.Options.SDKExtensions.
func sdkExtensionMounts(exts []builtSDKExtension) []server.SDKExtensionMount {
	out := make([]server.SDKExtensionMount, 0, len(exts))
	for _, e := range exts {
		out = append(out, server.SDKExtensionMount{Name: e.name, RegisterRoutes: e.ext.RegisterRoutes})
	}
	return out
}

// sdkExtensionTools joins every extension's Tools() into the agents' shared
// tool set, exactly like the GitHub extension's extTools does today.
func sdkExtensionTools(exts []builtSDKExtension) []tool.Tool {
	var out []tool.Tool
	for _, e := range exts {
		out = append(out, e.ext.Tools()...)
	}
	return out
}

// newExtDispatch builds the sdk.DispatchFunc an extension's Host carries.
// The prep (chat row, turn) is synchronous and fast; the run itself happens
// in a goroutine, so Dispatch returns before the run completes - RunObserver
// is how a caller learns it finished.
//
// Ask.NodeContext, Ask.ContextItems, Run.Setup, Run.ReadOnly, and
// Delivery.AllowedKinds have no consumer here yet - they await quack-core's
// Grant/CICheck generalization and the GitHub migration that's the real
// consumer of a pre-provisioned clone and node-scoped context.
func newExtDispatch(name string, orchRef *atomic.Pointer[orchestrator.Orchestrator], st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], shapes []workflowcatalog.Shape, artifacts *store.TurnAwareService) extsdk.DispatchFunc {
	return func(ctx context.Context, req extsdk.DispatchRequest) error {
		if req.Chat.LocalID == "" {
			return fmt.Errorf("extensions.%s: dispatch requires Chat.LocalID", name)
		}
		var shape workflowcatalog.Shape
		if req.Run.Workflow != "" {
			var ok bool
			shape, ok = workflowcatalog.Lookup(shapes, req.Run.Workflow)
			if !ok {
				return fmt.Errorf("extensions.%s: workflow %q is not in the configured workflow catalog", name, req.Run.Workflow)
			}
		}
		orch := orchRef.Load()
		if orch == nil {
			return fmt.Errorf("extensions.%s: orchestrator not ready", name)
		}
		// Namespaced so two extensions (or an extension and a user chat)
		// can never collide on the same global chat id.
		chatID := fmt.Sprintf("ext:%s:%s", name, req.Chat.LocalID)
		userID := req.Chat.User
		if userID == "" {
			userID = extRunUserID
		}

		// Detach from the HTTP request's lifecycle (the run outlives the
		// handler) while keeping the caller's trace, so the extension's
		// inbound span still parents the whole run's spans.
		runCtx := context.WithoutCancel(ctx)

		if req.Chat.ResetHistory {
			if err := orch.ResetSession(runCtx, userID, chatID); err != nil {
				return fmt.Errorf("extensions.%s: reset history: %w", name, err)
			}
		}

		originJSON := ""
		if req.Chat.Origin != nil {
			if b, err := json.Marshal(req.Chat.Origin); err == nil {
				originJSON = string(b)
			}
		}
		if err := st.SetChatOrigin(runCtx, chatID, userID, originJSON); err != nil {
			return fmt.Errorf("extensions.%s: chat setup: %w", name, err)
		}
		ensureExtChatTitle(runCtx, st, chatID, req.Chat.Title, req.Chat.Origin)

		turnID := uuid.NewString()
		if err := st.SaveTurn(runCtx, chatID, turnID); err != nil {
			slog.Warn("extension dispatch: save turn failed", "component", "ext."+name, "chat", chatID, "err", err)
		}

		attachments := make([]*genai.Part, 0, len(req.Ask.Attachments))
		for i, a := range req.Ask.Attachments {
			mime := a.MIME
			if mime == "" {
				mime = "application/octet-stream"
			}
			attName := a.Name
			if attName == "" {
				attName = fmt.Sprintf("attachment-%d", i)
			}
			ref, err := saveExtAttachment(runCtx, artifacts, userID, chatID, turnID, attName, a.Data, mime)
			if err != nil {
				slog.Warn("extension dispatch: attachment save failed; dropping this file",
					"component", "ext."+name, "chat", chatID, "name", attName, "err", err)
				continue
			}
			attachments = append(attachments, ref)
		}

		// A bound shape (Nodes non-empty) skips the planner LLM call entirely:
		// build the Plan now, synchronously, so a malformed binding is a hard
		// dispatch error - never a silent fallback to the unshaped hint path.
		if nodes, bound := workflowcatalog.Bind(shape, req.Ask.Message); bound {
			plan, err := orch.BuildBoundPlan(runCtx, nodes, req.Ask.Message, attachments)
			if err != nil {
				return fmt.Errorf("extensions.%s: workflow %q bound plan: %w", name, req.Run.Workflow, err)
			}
			go driveBoundExtensionRun(runCtx, name, orch, st, hub, extHolder, userID, chatID, turnID, *plan)
			return nil
		}

		go driveExtensionRun(runCtx, name, orch, st, hub, extHolder, userID, chatID, turnID, composeDispatchMessage(req), attachments)
		return nil
	}
}

// saveExtAttachment mirrors rest.Handler.saveAttachment: durably store the
// bytes and hand back a reference part (internal/artifactref), never the
// bytes, so plans/session events/the gen_ai ledger carry no attachment bytes.
func saveExtAttachment(ctx context.Context, artifacts *store.TurnAwareService, userID, chatID, turnID, name string, data []byte, mimeType string) (*genai.Part, error) {
	if artifacts == nil {
		return nil, fmt.Errorf("no artifact service configured")
	}
	resp, err := artifacts.SaveForTurn(ctx, &artifact.SaveRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: name,
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mimeType}},
	}, turnID)
	if err != nil {
		return nil, err
	}
	return artifactref.Encode(userID, chatID, name, resp.Version, mimeType), nil
}

// composeDispatchMessage folds the Workflow hint into Ask.Message. Only
// reached when Workflow names an unbound shape (no Nodes) or is empty - a
// bound shape never reaches here, it takes the BuildBoundPlan/
// driveBoundExtensionRun path instead. For an unbound shape this is still
// just a nudge: the orchestrator's own LLM turn reads the workflow-catalog
// table (internal/workflowcatalog) and decides what to do with it.
func composeDispatchMessage(req extsdk.DispatchRequest) string {
	msg := req.Ask.Message
	if req.Run.Workflow != "" {
		msg = fmt.Sprintf("Use the %q workflow shape from the workflow catalog (plan-work's Common workflows table) if one is configured with that name.\n\n%s", req.Run.Workflow, msg)
	}
	return msg
}

// ensureExtChatTitle sets a fresh chat's title - an explicit Chat.Title if
// given, else Origin.Label - mirroring internal/github's ensureTitle; never
// overwrites a title already set.
func ensureExtChatTitle(ctx context.Context, st *store.Store, chatID, title string, origin *extsdk.ChatOrigin) {
	if title == "" && origin != nil {
		title = origin.Label
	}
	if title == "" {
		return
	}
	c, err := st.GetChat(ctx, chatID)
	if err != nil || c == nil || c.Title != "" {
		return
	}
	_ = st.UpdateTitle(ctx, chatID, title)
}

// driveExtensionRun runs one dispatched turn to completion through the
// orchestrator's own LLM turn (the unshaped/hint path), mirroring
// rest.Handler.runChat / github.Extension.dispatch.
func driveExtensionRun(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID, message string, attachments []*genai.Part) {
	driveExtensionRunEvents(ctx, name, orch, st, hub, extHolder, userID, chatID, turnID, func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		return orch.Run(runCtx, userID, chatID, message, attachments)
	})
}

// driveBoundExtensionRun runs an already-built bound Plan to completion
// through RunBoundPlan - no orchestrator LLM turn, no planner LLM call.
func driveBoundExtensionRun(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID string, plan dag.Plan) {
	driveExtensionRunEvents(ctx, name, orch, st, hub, extHolder, userID, chatID, turnID, func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		return orch.RunBoundPlan(runCtx, userID, chatID, plan)
	})
}

// driveExtensionRunEvents drains one dispatched turn's SSE stream to
// completion, then fires RunEnded - the noop extension's dispatch counter
// only advances here, which is how the E2E test proves the whole
// register->route->dispatch->run loop actually ran. Shared by the unshaped
// (orch.Run) and bound (orch.RunBoundPlan) paths - identical bookkeeping
// either way, only the event source differs. run is called with runCtx (not
// ctx) so hub-driven cancellation (cancelRun) actually reaches the run.
func driveExtensionRunEvents(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID string, run func(context.Context) iter.Seq2[stream.SSEEvent, error]) {
	runCtx, cancelRun := context.WithCancel(ctx)
	hub.RegisterRun(chatID, turnID, cancelRun)
	_ = st.MarkRunActive(runCtx, chatID, turnID)
	defer hub.Close(chatID)
	defer func() {
		cancelRun()
		hub.UnregisterRun(chatID)
	}()

	eventLog := runlog.NewEventLog(st)
	eventLog.Reset(runCtx, chatID)
	pub := runlog.NewPublisher(hub, eventLog, chatID)
	pub.Publish(stream.ResponseCreated(turnID))

	var activePlanID string
	var needsInput stream.NodeNeedsInputData
	for ev, err := range run(runCtx) {
		if err != nil {
			slog.Warn("extension run error", "component", "ext."+name, "chat", chatID, "err", err)
			continue
		}
		if ev.Name == stream.EventDagPlan {
			if d, ok := ev.Data.(stream.DagPlanData); ok {
				activePlanID = d.PlanID
				runlog.SaveDagPlan(st, chatID, turnID, d)
			}
		} else if activePlanID != "" {
			runlog.PersistNodeEvent(st, activePlanID, ev)
		}
		if ev.Name == stream.EventNodeNeedsInput {
			if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
				needsInput = d
			}
		}
		pub.Publish(ev)
	}
	pub.Publish(stream.Done())

	outcome := buildExtRunOutcome(runCtx, orch, st, userID, chatID, activePlanID != "", needsInput)
	if p := extHolder.Load(); p != nil {
		if obs, ok := (*p).(extsdk.RunObserver); ok {
			obs.RunEnded(chatID, outcome)
		}
	}
}

// buildExtRunOutcome mirrors rest.Handler.stampRunOutcome / github's
// stampRunOutcome (#738's terminal-status rule) and additionally builds the
// RunOutcome RunObserver expects. TimedOut always reports false: extension
// dispatch has no per-run deadline wired yet (only the GitHub webhook path
// sets one) - that lands with the GitHub migration.
func buildExtRunOutcome(parent context.Context, orch *orchestrator.Orchestrator, st *store.Store, userID, chatID string, planRan bool, needsInput stream.NodeNeedsInputData) extsdk.RunOutcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	turns, err := st.GetTurnsWithContent(ctx, orchestrator.AppName, userID, chatID)
	if err != nil {
		slog.Warn("extension: stamp run outcome: turns load failed", "component", "serve", "chat", chatID, "err", err)
	}
	q, hasQ := orch.PendingQuestion(ctx, userID, chatID)
	status, question := store.DeriveTerminalStatus(turns, q, hasQ)
	if err := st.StampRunOutcome(ctx, chatID, status, question); err != nil {
		slog.Warn("extension: stamp run outcome failed", "component", "serve", "chat", chatID, "err", err)
	}

	out := extsdk.RunOutcome{PlanRan: planRan, Answer: strings.TrimSpace(orch.LatestAnswer(ctx, userID, chatID))}
	switch status {
	case store.RunStatusFailed:
		out.Status = extsdk.RunFailed
	case store.RunStatusNeedsInput:
		out.Status = extsdk.RunNeedsInput
		out.Question = question
		out.NodeID = needsInput.NodeID
	default:
		out.Status = extsdk.RunDone
	}
	return out
}
