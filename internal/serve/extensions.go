package serve

import (
	"context"
	"encoding/json"
	"errors"
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
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// extRunUserID distinguishes extension-dispatched sessions from local UI and
// GitHub-driven ones, mirroring internal/github's runUserID convention.
const extRunUserID = "ext"

// builtSDKExtension pairs a constructed quack-extensions SDK module with the
// registration name its config and log lines are keyed by.
type builtSDKExtension struct {
	name string
	ext  extsdk.Extension
	// title/href/icon come from the module's optional sdk.UI descriptor,
	// captured once at build time; all empty when the module implements no
	// UI (the SPA nav then lists it name-only).
	title string
	href  string
	icon  string
}

// buildSDKExtensions constructs every configured module named under
// extensions: that is also compiled in (sdk.Registered(), populated by
// extensions_registry.go's blank imports). A configured name absent from the
// registry, or one that fails ValidateExtensionName, fails startup loudly; a
// registered module absent from config, or configured with enabled: false,
// stays dormant - never constructed (design doc "Model"). orchRef and
// judgeModelRef are read lazily by the returned extensions' Dispatch/Classify
// closures: neither is resolved until the caller Stores it, both built later
// in buildFromConfig (judgeModelRef may never be Stored at all when no judge
// model is configured - Classify degrades to an error, matching Host's own
// nil-is-valid contract). taskMem/userMem are already-built by the time this
// runs (buildFromConfig constructs them first) and may each be nil - the same
// task/user split rest/memory.go's memStores() iterates.
func buildSDKExtensions(cfg *config.Config, st *store.Store, hub *stream.Hub, orchRef *atomic.Pointer[orchestrator.Orchestrator], artifacts *store.TurnAwareService, jail *workspace.Jail, judgeModelRef *atomic.Pointer[model.LLM], taskMem, userMem *memory.Store) ([]builtSDKExtension, error) {
	factories := extsdk.Registered()
	names := make([]string, 0, len(cfg.Extensions.Modules))
	for name := range cfg.Extensions.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

	shapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)

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
			EnsureContextDir: func(userID, chatID string) (string, error) {
				return jail.EnsureDir(userID, chatID, workspace.ContextDirScope)
			},
			ChatUser: func(chatID string) (string, bool) {
				u := st.SessionUserForChat(context.Background(), chatID)
				return u, u != ""
			},
			ArchiveChat: func(chatID string) error {
				return st.ArchiveChat(context.Background(), chatID, true)
			},
			UpdateChatOrigin: newExtUpdateChatOrigin(name, st, taskMem, userMem),
			InvalidateSetup: func(chatID string) error {
				dag.MarkSetupStale(chatID)
				return nil
			},
			Classify: func(ctx context.Context, prompt string) (string, error) {
				m := judgeModelRef.Load()
				if m == nil || *m == nil {
					return "", fmt.Errorf("extensions.%s: classify: no judge model configured", name)
				}
				return classifyWithModel(ctx, *m, prompt)
			},
		}
		ext, err := factory(host, raw)
		if err != nil {
			return nil, fmt.Errorf("extensions.%s: factory: %w", name, err)
		}
		extHolder.Store(&ext)
		b := builtSDKExtension{name: name, ext: ext}
		if ui, ok := ext.(extsdk.UI); ok {
			d := ui.UI()
			b.title, b.href, b.icon = d.Title, d.Href, d.Icon
		}
		built = append(built, b)
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

// extensionDescriptors adapts built extensions onto the GET /api/v1/extensions
// response shape - name-only unless the module implements sdk.UI.
func extensionDescriptors(exts []builtSDKExtension) []schema.ExtensionInfo {
	out := make([]schema.ExtensionInfo, 0, len(exts))
	for _, e := range exts {
		info := schema.ExtensionInfo{Name: e.name}
		if e.title != "" {
			info.Title = &e.title
		}
		if e.href != "" {
			info.Href = &e.href
		}
		if e.icon != "" {
			info.Icon = &e.icon
		}
		out = append(out, info)
	}
	return out
}

// sdkExtensionTools joins every extension's Tools() into the agents' shared
// tool set, exactly like the GitHub extension's extTools does today.
func sdkExtensionTools(exts []builtSDKExtension) []extTool {
	var out []extTool
	for _, e := range exts {
		for _, t := range e.ext.Tools() {
			out = append(out, extTool{provider: e.name, tool: t})
		}
	}
	return out
}

// findGitCredentialSource returns the first built extension implementing
// sdk.GitCredentialSource, detected the same way Starter/Stopper are - not
// hardcoded to one extension's name. More than one match logs a warning and
// keeps the first (deterministic build order, sorted by name); today only
// the GitHub extension implements this.
func findGitCredentialSource(exts []builtSDKExtension) (extsdk.GitCredentialSource, string) {
	var found extsdk.GitCredentialSource
	var foundName string
	for _, e := range exts {
		src, ok := e.ext.(extsdk.GitCredentialSource)
		if !ok {
			continue
		}
		if found != nil {
			slog.Warn("multiple extensions implement GitCredentialSource; keeping the first",
				"component", "startup", "using", foundName, "ignoring", e.name)
			continue
		}
		found, foundName = src, e.name
	}
	return found, foundName
}

// findDeliverer returns the first built extension implementing sdk.Deliverer -
// same detection/ambiguity rule as findGitCredentialSource.
func findDeliverer(exts []builtSDKExtension) (extsdk.Deliverer, string) {
	var found extsdk.Deliverer
	var foundName string
	for _, e := range exts {
		d, ok := e.ext.(extsdk.Deliverer)
		if !ok {
			continue
		}
		if found != nil {
			slog.Warn("multiple extensions implement Deliverer; keeping the first",
				"component", "startup", "using", foundName, "ignoring", e.name)
			continue
		}
		found, foundName = d, e.name
	}
	return found, foundName
}

// sdkGitCredentialAdapter bridges sdk.GitCredentialSource to
// tools.GitTokenSource - same shape, different concrete credential type
// (the SDK boundary can't share quack's own internal type).
type sdkGitCredentialAdapter struct{ src extsdk.GitCredentialSource }

func (a sdkGitCredentialAdapter) GitCredential(ctx context.Context, rawURL string) (*tools.GitCredential, error) {
	c, err := a.src.GitCredential(ctx, rawURL)
	if err != nil || c == nil {
		return nil, err
	}
	return &tools.GitCredential{Host: c.Host, Username: c.Username, Token: c.Token}, nil
}

// sdkDeliverAdapter bridges sdk.Deliverer to vetting.DeliverFunc.
type sdkDeliverAdapter struct{ deliverer extsdk.Deliverer }

func (a sdkDeliverAdapter) Deliver(ctx context.Context, dc vetting.DeliveryContext) ([]vetting.DeliveryItemOutcome, error) {
	sdkItems := make([]extsdk.StagedDelivery, len(dc.Items))
	for i, it := range dc.Items {
		comments := make([]extsdk.ReviewComment, len(it.Comments))
		for j, c := range it.Comments {
			comments[j] = extsdk.ReviewComment{Path: c.Path, Line: c.Line, Body: c.Body}
		}
		sdkItems[i] = extsdk.StagedDelivery{
			Kind: extsdk.DeliveryKind(it.Kind), Branch: it.Branch, Title: it.Title, Body: it.Body,
			TitleOmitted: it.TitleOmitted, BodyOmitted: it.BodyOmitted,
			Event: it.Event, Slot: it.Slot, Comments: comments, Recovered: it.Recovered,
		}
	}
	outcomes, err := a.deliverer.Deliver(ctx, extsdk.DeliveryContext{
		NodeID: dc.NodeID, ChatID: dc.ChatID, Items: sdkItems,
		CloneURL: dc.CloneURL, PushedSHA: dc.PushedSHA, Branch: dc.Branch, IssueNumber: dc.IssueNumber,
		GatePassed: dc.GatePassed, GateFeedback: dc.GateFeedback, ChecksSkipNote: dc.ChecksSkipNote,
	})
	out := make([]vetting.DeliveryItemOutcome, len(outcomes))
	for i, o := range outcomes {
		out[i] = vetting.DeliveryItemOutcome{Kind: o.Kind, URL: o.URL, Error: o.Error}
	}
	return out, err
}

// newExtDispatch builds the sdk.DispatchFunc an extension's Host carries.
// The prep (chat row, turn) is synchronous and fast; the run itself happens
// in a goroutine, so Dispatch returns before the run completes - RunObserver
// is how a caller learns it finished.
func newExtDispatch(name string, orchRef *atomic.Pointer[orchestrator.Orchestrator], st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], shapes []workflowcatalog.Shape, artifacts *store.TurnAwareService) extsdk.DispatchFunc {
	return func(ctx context.Context, req extsdk.DispatchRequest) error {
		if hub.Draining() {
			return fmt.Errorf("extensions.%s: server is shutting down; dispatch will need to be retried", name)
		}
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
		// inbound span still parents the whole run's spans. Run.Timeout is
		// applied inside driveExtensionRunEvents, which already owns
		// runCtx's cancel-func lifecycle end to end.
		runCtx := context.WithoutCancel(ctx)
		allowedKinds := deliveryKindStrings(req.Delivery.AllowedKinds)

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
			plan, err := orch.BuildBoundPlan(runCtx, nodes, req.Ask.Message, attachments, allowedKinds)
			if err != nil {
				return fmt.Errorf("extensions.%s: workflow %q bound plan: %w", name, req.Run.Workflow, err)
			}
			go driveBoundExtensionRun(runCtx, name, orch, st, hub, extHolder, userID, chatID, turnID, *plan, req.Run.Timeout)
			return nil
		}

		// The unshaped/hint path reaches the planner's own LLM turn (plan
		// tool), which reads these facts back off ctx - see
		// tools.AllowedDeliveryKindsFromContext, GitHubSetupFromContext,
		// WorkerAskFromContext, ContextItemsFromContext, PlanOnlyFromContext.
		// The real consumer of Setup/NodeContext/ContextItems/ReadOnly - the
		// GitHub migration's pre-provisioned clone and node-scoped context.
		runCtx = tools.WithAllowedDeliveryKinds(runCtx, allowedKinds)
		if req.Run.Setup != nil {
			runCtx = tools.WithGitHubSetup(runCtx, toDagSetup(*req.Run.Setup))
		}
		if req.Ask.NodeContext != "" {
			runCtx = tools.WithWorkerAsk(runCtx, req.Ask.NodeContext)
		}
		if req.Ask.ContextItems != nil {
			runCtx = tools.WithContextItems(runCtx, toDagContextItems(req.Ask.ContextItems))
		}
		runCtx = tools.WithPlanOnly(runCtx, req.Run.ReadOnly)
		go driveExtensionRun(runCtx, name, orch, st, hub, extHolder, userID, chatID, turnID, composeDispatchMessage(req), attachments, req.Run.Timeout)
		return nil
	}
}

// deliveryKindStrings converts the SDK's typed delivery-kind list to the
// bare-string vocabulary vetting.Config.AllowedDeliveryKinds and dag.Plan
// share; nil stays nil (unrestricted - see AllowedDeliveryKinds' own doc).
func deliveryKindStrings(kinds []extsdk.DeliveryKind) []string {
	if kinds == nil {
		return nil
	}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// toDagSetup adapts the SDK's Setup to dag.Setup. ExistingHeadRef overrides
// WorkBranch for the checkout rather than supplementing it (mirrors what
// dag.OverrideExistingPRHead used to do post-hoc from the GitHub-specific
// tools.WithGitHubPR context - now folded into Setup itself, generalized
// past GitHub).
func toDagSetup(s extsdk.Setup) dag.Setup {
	out := dag.Setup{Repo: s.Repo, BaseRef: s.BaseRef, WorkBranch: s.WorkBranch}
	if s.ExistingHeadRef != "" {
		out.WorkBranch = s.ExistingHeadRef
		out.CheckoutExistingHead = true
	}
	return out
}

// toDagContextItems adapts the SDK's NamedContext to dag.ContextItem - same shape.
func toDagContextItems(items []extsdk.NamedContext) []dag.ContextItem {
	out := make([]dag.ContextItem, len(items))
	for i, it := range items {
		out[i] = dag.ContextItem{Name: it.Name, Detail: it.Detail}
	}
	return out
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

// newExtUpdateChatOrigin builds the sdk.Host.UpdateChatOrigin closure: same
// "ext:<name>:<localID>" namespacing newExtDispatch uses, and the same
// origin marshaling newExtDispatch does for a fresh dispatch. Returns
// extsdk.ErrUnknownChat when localID never reached Dispatch, so a
// state-change webhook for an issue/PR that was never dispatched (the common
// case) fails predictably rather than silently minting a bare chat row.
//
// Also where memory-lifecycle design doc §4(b)/§5's core-side interpretation
// lives: the extension only ever reports the domain fact (State); mapping a
// State transition to a memory outcome, and which stores that outcome
// touches, is core's own call - memory concepts never cross the SDK
// boundary. taskMem/userMem stay nil-tolerant like every other Host field.
func newExtUpdateChatOrigin(name string, st *store.Store, taskMem, userMem *memory.Store) func(localID string, origin extsdk.ChatOrigin) error {
	return func(localID string, origin extsdk.ChatOrigin) error {
		chatID := fmt.Sprintf("ext:%s:%s", name, localID)
		ctx := context.Background()
		c, err := st.GetChat(ctx, chatID)
		if err != nil {
			return fmt.Errorf("extensions.%s: update chat origin: %w", name, err)
		}
		if c == nil {
			return fmt.Errorf("extensions.%s: update chat origin: %w", name, extsdk.ErrUnknownChat)
		}
		prevState := priorOriginState(c.Origin)
		b, err := json.Marshal(&origin)
		if err != nil {
			return fmt.Errorf("extensions.%s: update chat origin: marshal: %w", name, err)
		}
		if err := st.SetChatOrigin(ctx, chatID, c.SessionUser, string(b)); err != nil {
			return fmt.Errorf("extensions.%s: update chat origin: %w", name, err)
		}
		applyMemoryOutcome(ctx, name, chatID, prevState, origin.State, taskMem, userMem)
		return nil
	}
}

// priorOriginState reads State off a chat's previously stored origin JSON
// (opaque to internal/store - see Chat.Origin), so newExtUpdateChatOrigin can
// tell a transition from steady state before overwriting it. "" (including
// no prior origin, or one minted before sdk v0.5.0 added State) reads as
// unknown, same as the SDK's own zero value.
func priorOriginState(originJSON string) extsdk.SubjectState {
	if originJSON == "" {
		return ""
	}
	var o extsdk.ChatOrigin
	if err := json.Unmarshal([]byte(originJSON), &o); err != nil {
		return ""
	}
	return o.State
}

// applyMemoryOutcome maps a ChatOrigin.State transition to a memory outcome
// (design doc §4(b)/§5): merged reinforces, closed (from anything) invalidates
// with a fixed reason, open/"" is a no-op either direction - stickiness
// against a reopen-after-close lives in memory.Store.ApplyOutcome itself, not
// here. Steady state (prev == next, e.g. a repeated closed webhook) never
// reaches an outcome. Fire-and-forget: a memory store error is logged, never
// surfaced - the origin update it rides on must not fail because of it.
func applyMemoryOutcome(ctx context.Context, name, chatID string, prev, next extsdk.SubjectState, stores ...*memory.Store) {
	if prev == next {
		return
	}
	var outcome memory.OutcomeSignal
	switch next {
	case extsdk.SubjectMerged:
		outcome = memory.OutcomeSignal{Kind: memory.OutcomeReinforced}
	case extsdk.SubjectClosed:
		outcome = memory.OutcomeSignal{Kind: memory.OutcomeInvalidated, Reason: memory.OutcomeReasonClosedUnmerged}
	default:
		return
	}
	for _, s := range stores {
		if s == nil {
			continue
		}
		if _, err := s.ApplyOutcome(ctx, chatID, outcome); err != nil {
			slog.Warn("apply memory outcome failed", "component", "ext."+name, "chat", chatID, "kind", outcome.Kind, "err", err)
		}
	}
}

// driveExtensionRun runs one dispatched turn to completion through the
// orchestrator's own LLM turn (the unshaped/hint path), mirroring
// rest.Handler.runChat / github.Extension.dispatch. timeout is
// Run.Timeout - zero means unbounded.
func driveExtensionRun(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID, message string, attachments []*genai.Part, timeout time.Duration) {
	driveExtensionRunEvents(ctx, name, orch, st, hub, extHolder, userID, chatID, turnID, timeout, func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		// name doubles as the token.usage/cost "source" attribution - the
		// extension's own registration name (github, remarkable, ...).
		return orch.Run(runCtx, userID, chatID, name, message, attachments)
	})
}

// driveBoundExtensionRun runs an already-built bound Plan to completion
// through RunBoundPlan - no orchestrator LLM turn, no planner LLM call.
func driveBoundExtensionRun(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID string, plan dag.Plan, timeout time.Duration) {
	driveExtensionRunEvents(ctx, name, orch, st, hub, extHolder, userID, chatID, turnID, timeout, func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
		return orch.RunBoundPlan(runCtx, userID, chatID, name, plan)
	})
}

// driveExtensionRunEvents drains one dispatched turn's SSE stream to
// completion, then fires RunEnded - the noop extension's dispatch counter
// only advances here, which is how the E2E test proves the whole
// register->route->dispatch->run loop actually ran. Shared by the unshaped
// (orch.Run) and bound (orch.RunBoundPlan) paths - identical bookkeeping
// either way, only the event source differs. run is called with runCtx (not
// ctx) so hub-driven cancellation (cancelRun) actually reaches the run.
// timeout>0 bounds runCtx itself (Run.Timeout) so TimedOut is observable
// below - cancelRun (deferred) is what actually releases it either way.
func driveExtensionRunEvents(ctx context.Context, name string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, extHolder *atomic.Pointer[extsdk.Extension], userID, chatID, turnID string, timeout time.Duration, run func(context.Context) iter.Seq2[stream.SSEEvent, error]) {
	var runCtx context.Context
	var cancelRun context.CancelFunc
	if timeout > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, timeout)
	} else {
		runCtx, cancelRun = context.WithCancel(ctx)
	}
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

	res := runlog.Drive(turnID, st, pub, run(runCtx), func(err error) {
		slog.Warn("extension run error", "component", "ext."+name, "chat", chatID, "err", err)
	})
	pub.Publish(stream.Done())
	// Stamp model + usage on the turn row - rest.Handler.runChat shares this
	// same tail, so an extension-dispatched chat gets it too.
	runlog.StampTurn(runCtx, st, chatID, turnID, res)

	// Shutdown force-cancelled this run - skip RunEnded entirely so a deploy
	// never posts a PR comment for work the process didn't get to finish.
	// Per-chat marker, not global Draining: a run that finishes normally
	// during the drain window keeps its RunEnded. The drain pauses nodes
	// (#962), so the chat stamps paused and boot resumes it.
	if hub.WasInterrupted(chatID) {
		stampCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := st.StampRunOutcome(stampCtx, chatID, store.RunStatusPaused, ""); err != nil {
			slog.Warn("extension run: interrupted stamp failed", "component", "ext."+name, "chat", chatID, "err", err)
		}
		cancel()
		slog.Warn("extension run cut by shutdown; no RunEnded delivered", "component", "ext."+name, "chat", chatID)
		return
	}

	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	// hub.RegisterRun's cancel func is runCtx's own - a user Stop surfaces here as Canceled.
	cancelled := errors.Is(runCtx.Err(), context.Canceled)
	outcome := buildExtRunOutcome(runCtx, orch, st, userID, chatID, res.PlanID != "", res.NeedsInput, timedOut, cancelled)
	if p := extHolder.Load(); p != nil {
		if obs, ok := (*p).(extsdk.RunObserver); ok {
			obs.RunEnded(chatID, outcome)
		}
	}
}

// buildExtRunOutcome mirrors rest.Handler.stampRunOutcome / github's
// stampRunOutcome (#738's terminal-status rule) and additionally builds the
// RunOutcome RunObserver expects. timedOut is the caller's own
// Run.Timeout-scoped deadline check (buildExtRunOutcome's own ctx is
// deliberately WithoutCancel of parent, so it can't observe parent's
// deadline itself).
func buildExtRunOutcome(parent context.Context, orch *orchestrator.Orchestrator, st *store.Store, userID, chatID string, planRan bool, needsInput stream.NodeNeedsInputData, timedOut, cancelled bool) extsdk.RunOutcome {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	status, question, nodeError := st.StampTerminalOutcome(ctx, orchestrator.AppName, userID, chatID, func() (string, bool) {
		return orch.PendingQuestion(ctx, userID, chatID)
	})
	answer := strings.TrimSpace(orch.LatestAnswer(ctx, userID, chatID))
	return mapExtRunOutcome(status, question, nodeError, answer, planRan, needsInput, timedOut, cancelled)
}

// mapExtRunOutcome is buildExtRunOutcome's classification step, split out so
// it's testable without a live store/orch. cancelled wins over status - it
// interrupted whatever DeriveTerminalStatus derived from the turn. nodeError
// is the failed node's own error text (#1105) - "" for a true silent gap.
func mapExtRunOutcome(status, question, nodeError, answer string, planRan bool, needsInput stream.NodeNeedsInputData, timedOut, cancelled bool) extsdk.RunOutcome {
	out := extsdk.RunOutcome{PlanRan: planRan, TimedOut: timedOut, Answer: answer}
	switch {
	case cancelled:
		out.Status = extsdk.RunCancelled
	case status == store.RunStatusFailed:
		out.Status = extsdk.RunFailed
		// extsdk.RunOutcome has no Error field yet (a needed sdk follow-up,
		// see #1105) - fold the failed node's real cause into Answer so an
		// empty answer never falls through to the extension's silent-gap
		// text for a run that in fact failed with a known cause. nodeError
		// is already sanitized (dag.emptyNodeError via
		// inference.SanitizeGatewayError) - no raw URL/body/key reaches here.
		if out.Answer == "" && nodeError != "" {
			guidance := "Check the model gateway / provider configuration."
			if inference.TransientFromSummary(nodeError) {
				guidance = "Retry once the gateway is healthy."
			}
			out.Answer = fmt.Sprintf("quack's run failed: %s\n\n%s", nodeError, guidance)
		}
	case status == store.RunStatusNeedsInput:
		out.Status = extsdk.RunNeedsInput
		out.Question = question
		out.NodeID = needsInput.NodeID
	default:
		out.Status = extsdk.RunDone
		if out.Answer == "" && !timedOut {
			// Silent-gap (#568): a run that finished with no error, no failed
			// node, and no answer. Was GitHub-only (internal/github's own
			// call); centralized here so every extension's dispatch gets the
			// same metric, matching rest.Handler's own runs once it adopts
			// this same accounting.
			otelobs.RecordRunNoAnswer()
		}
	}
	return out
}
