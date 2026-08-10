package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
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
// registry fails startup loudly; a registered module absent from config
// stays dormant - never constructed (design doc "Model"). orchRef is read
// lazily by the returned extensions' Dispatch closures: it isn't resolved
// until the caller Stores the orchestrator, built later in buildFromConfig.
func buildSDKExtensions(cfg *config.Config, st *store.Store, hub *stream.Hub, orchRef *atomic.Pointer[orchestrator.Orchestrator]) ([]builtSDKExtension, error) {
	factories := extsdk.Registered()
	names := make([]string, 0, len(cfg.Extensions.Modules))
	for name := range cfg.Extensions.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

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
		node := cfg.Extensions.Modules[name]
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("extensions.%s: re-marshal config: %w", name, err)
		}
		dataDir := filepath.Join(cfg.Workspace.Root, "extensions", name)
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("extensions.%s: data dir: %w", name, err)
		}

		var extHolder atomic.Pointer[extsdk.Extension]
		host := extsdk.Host{
			Dispatch: newExtDispatch(name, orchRef, st, hub, &extHolder),
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
func newExtDispatch(name string, orchRef *atomic.Pointer[orchestrator.Orchestrator], st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension]) extsdk.DispatchFunc {
	return func(ctx context.Context, req extsdk.DispatchRequest) error {
		if req.ChatID == "" {
			return fmt.Errorf("extensions.%s: dispatch requires a ChatID", name)
		}
		orch := orchRef.Load()
		if orch == nil {
			return fmt.Errorf("extensions.%s: orchestrator not ready", name)
		}
		userID := req.User
		if userID == "" {
			userID = extRunUserID
		}

		originJSON := ""
		if req.Origin != nil {
			if b, err := json.Marshal(req.Origin); err == nil {
				originJSON = string(b)
			}
		}
		// Detach from the HTTP request's lifecycle (the run outlives the
		// handler) while keeping the caller's trace, so the extension's
		// inbound span still parents the whole run's spans.
		runCtx := context.WithoutCancel(ctx)
		if err := st.SetChatOrigin(runCtx, req.ChatID, userID, originJSON); err != nil {
			return fmt.Errorf("extensions.%s: chat setup: %w", name, err)
		}
		ensureExtChatTitle(runCtx, st, req.ChatID, req.Origin)

		attachments := make([]*genai.Part, 0, len(req.Attachments))
		for _, a := range req.Attachments {
			mime := a.MIME
			if mime == "" {
				mime = "application/octet-stream"
			}
			attachments = append(attachments, &genai.Part{InlineData: &genai.Blob{Data: a.Data, MIMEType: mime}})
		}

		turnID := uuid.NewString()
		if err := st.SaveTurn(runCtx, req.ChatID, turnID); err != nil {
			slog.Warn("extension dispatch: save turn failed", "component", "ext."+name, "chat", req.ChatID, "err", err)
		}

		go driveExtensionRun(runCtx, name, orch, st, hub, extHolder, userID, req.ChatID, turnID, composeDispatchMessage(req), attachments)
		return nil
	}
}

// composeDispatchMessage folds Background and Workflow into Message. Workflow
// is a hint, not a binding: the planner is one LLM call that reads the
// workflow-catalog table itself (internal/workflowcatalog) - there is no
// programmatic shape selector today, so naming one here only nudges that call.
func composeDispatchMessage(req extsdk.DispatchRequest) string {
	msg := req.Message
	if req.Workflow != "" {
		msg = fmt.Sprintf("Use the %q workflow shape from the workflow catalog (plan-work's Common workflows table) if one is configured with that name.\n\n%s", req.Workflow, msg)
	}
	if req.Background != "" {
		msg = req.Background + "\n\n---\n\n" + msg
	}
	return msg
}

// ensureExtChatTitle sets a fresh chat's title from Origin.Label, mirroring
// internal/github's ensureTitle - never overwrites a title already set.
func ensureExtChatTitle(ctx context.Context, st *store.Store, chatID string, origin *extsdk.ChatOrigin) {
	if origin == nil || origin.Label == "" {
		return
	}
	c, err := st.GetChat(ctx, chatID)
	if err != nil || c == nil || c.Title != "" {
		return
	}
	_ = st.UpdateTitle(ctx, chatID, origin.Label)
}

// driveExtensionRun runs one dispatched turn to completion, mirroring
// rest.Handler.runChat / github.Extension.dispatch, then fires RunEnded -
// the noop extension's dispatch counter only advances here, which is how the
// E2E test proves the whole register->route->dispatch->run loop actually ran.
func driveExtensionRun(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID, message string, attachments []*genai.Part) {
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
	for ev, err := range orch.Run(runCtx, userID, chatID, message, attachments) {
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
		pub.Publish(ev)
	}
	pub.Publish(stream.Done())

	status := stampExtRunOutcome(runCtx, orch, st, userID, chatID)
	if p := extHolder.Load(); p != nil {
		if obs, ok := (*p).(extsdk.RunObserver); ok {
			obs.RunEnded(chatID, status)
		}
	}
}

// stampExtRunOutcome mirrors rest.Handler.stampRunOutcome / github's
// stampRunOutcome (#738's terminal-status rule), returning the sdk.RunStatus
// RunObserver expects.
func stampExtRunOutcome(parent context.Context, orch *orchestrator.Orchestrator, st *store.Store, userID, chatID string) extsdk.RunStatus {
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
	switch status {
	case store.RunStatusFailed:
		return extsdk.RunFailed
	case store.RunStatusNeedsInput:
		return extsdk.RunNeedsInput
	default:
		return extsdk.RunDone
	}
}
