package tools

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

const toolsScope = "quack.tools"

// cancelledMsg: instruction to stop, not a diagnostic (retry loops defeat cancellation).
const cancelledMsg = "This node was CANCELLED by the user. Stop calling tools. End your turn now with whatever you have."

// HookPolicy picks which per-call checks Hooks runs for a tool.
type HookPolicy uint8

const (
	// HookRepeat refuses identical-call loops and resource-failure churn.
	HookRepeat HookPolicy = 1 << iota
	// HookEmit records one execute_tool ledger row per call.
	HookEmit
	// HookNode adds the cancelled-node refusal, host-path error scrubbing and the stamped ledger coords.
	HookNode
	// HookBuilt is what Build's own tools get.
	HookBuilt = HookRepeat | HookEmit | HookNode
)

// Hooks runs quack's per-call tool policy as llmagent tool callbacks. One instance per agent, so every
// tool it covers spends the same repeat budget.
type Hooks struct {
	repeats   *repeatStates
	tripped   func(chatID, nodeID, msg string) bool
	cancelled func(chatID, nodeID string) bool
	scope     CallScope
	scrub     *fsBinding
	fallback  HookPolicy
	policy    map[string]HookPolicy

	mu     sync.Mutex
	coords ledger.Coords
}

// NewHooks reads NodeCancelled, RepeatGuardTripped, CallScope and the workspace binding from d; fallback
// covers tools never passed to Set, e.g. a toolset's.
func NewHooks(d Deps, fallback HookPolicy) *Hooks {
	h := &Hooks{repeats: newRepeatStates(), tripped: d.RepeatGuardTripped, cancelled: d.NodeCancelled,
		scope: d.CallScope, fallback: fallback, policy: map[string]HookPolicy{}}
	if b, err := newFSBinding(d); err == nil {
		h.scrub = &b
	}
	return h
}

// Set assigns p to ts by name; call it before the agent runs.
func (h *Hooks) Set(p HookPolicy, ts ...tool.Tool) {
	for _, t := range ts {
		h.policy[t.Name()] = p
	}
}

func (h *Hooks) policyOf(name string) HookPolicy {
	if p, ok := h.policy[name]; ok {
		return p
	}
	return h.fallback
}

// SetLedgerCoords stamps the coords HookNode tools' ledger rows fall back on: a node's A2A-served ctx carries none.
func (h *Hooks) SetLedgerCoords(c ledger.Coords) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.coords = c
	h.mu.Unlock()
}

// Wire appends h's callbacks to cfg; its Before runs last so earlier callbacks still see every call.
func (h *Hooks) Wire(cfg *llmagent.Config) {
	if h == nil {
		return
	}
	cfg.BeforeToolCallbacks = append(cfg.BeforeToolCallbacks, h.before)
	cfg.OnToolErrorCallbacks = append(cfg.OnToolErrorCallbacks, h.onError)
	cfg.AfterToolCallbacks = append(cfg.AfterToolCallbacks, h.after)
}

// refusal is a call the hooks stopped before the tool ran; After skips repeat bookkeeping for it.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refused(err error) bool {
	var r *refusal
	return errors.As(err, &r)
}

// before drops nulls the tool's schema refuses, refuses a cancelled node's call (calls without node scope,
// or whose thread is gone, never are), then runs the repeat check.
func (h *Hooks) before(ctx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	if d, ok := t.(interface {
		Declaration() *genai.FunctionDeclaration
	}); ok {
		if s, ok := d.Declaration().ParametersJsonSchema.(*jsonschema.Schema); ok {
			dropRefusedNulls(args, s)
		}
	}
	p := h.policyOf(t.Name())
	if p&HookNode != 0 && h.cancelled != nil {
		if chatID, nodeID := h.scope.node(ctx); nodeID != "" && h.cancelled(chatID, nodeID) {
			slog.Info("tool call refused: node cancelled by user", "component", "tools",
				"tool", t.Name(), "chat", chatID, "node", nodeID)
			return nil, &refusal{cancelledMsg}
		}
	}
	if p&HookRepeat != 0 {
		return nil, h.checkRepeat(ctx, t.Name(), args)
	}
	return nil, nil
}

// dropRefusedNulls deletes, in place, each null at an optional key whose schema refuses null: models
// send null to mean "not set", and the tool reads an absent key the same way.
func dropRefusedNulls(args map[string]any, s *jsonschema.Schema) {
	for k, v := range args {
		p, ok := s.Properties[k]
		if !ok {
			p = s.AdditionalProperties
		}
		if v == nil && !slices.Contains(s.Required, k) && refusesNull(p) {
			delete(args, k)
			continue
		}
		dropNestedNulls(v, p)
	}
}

func dropNestedNulls(v any, s *jsonschema.Schema) {
	if s == nil {
		return
	}
	switch v := v.(type) {
	case map[string]any:
		dropRefusedNulls(v, s)
	case []any:
		for _, x := range v {
			dropNestedNulls(x, s.Items)
		}
	}
}

// refusesNull is true for a typed schema without "null" and for the false schema
// (an unknown key under additionalProperties: false); nil and untyped schemas accept null.
func refusesNull(s *jsonschema.Schema) bool {
	switch {
	case s == nil:
		return false
	case s.Not != nil && reflect.ValueOf(*s.Not).IsZero():
		return true
	case s.Type != "":
		return s.Type != "null"
	default:
		return len(s.Types) > 0 && !slices.Contains(s.Types, "null")
	}
}

// onError respells workspace host paths in a tool's own error into the model's namespace.
func (h *Hooks) onError(ctx agent.Context, t tool.Tool, _ map[string]any, err error) (map[string]any, error) {
	if h.scrub == nil || h.policyOf(t.Name())&HookNode == 0 || refused(err) {
		return nil, nil
	}
	if s := scrubHostPaths(err, h.scrub.jail.Root(), h.scrub.withCwd(ctx).workRoot()); s != err { //nolint:errorlint // identity: scrubHostPaths returns err itself when nothing was rewritten
		return nil, s
	}
	return nil, nil
}

// after folds a call that ran into the repeat state, then emits its ledger row; it never changes the result.
func (h *Hooks) after(ctx agent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
	p := h.policyOf(t.Name())
	if p&HookRepeat != 0 && !refused(err) {
		h.recordRepeat(ctx, t.Name(), args, result, err)
	}
	// Counted whatever the policy: artifact tools (write_<kind>) emit no row here, yet their refusals cost a request.
	otelobs.RecordToolCall(h.callCoords(ctx), t.Name(), err != nil)
	if p&HookEmit == 0 {
		return nil, nil
	}
	emitCtx := context.Context(ctx)
	if p&HookNode != 0 {
		emitCtx = ledger.WithCoords(ctx, h.callCoords(ctx))
	}
	otelobs.EmitToolCall(emitCtx, toolsScope, t.Name(), args, result, err)
	return nil, nil
}

// callCoords is ctx's coords with the stamped ones filling its blanks.
func (h *Hooks) callCoords(ctx context.Context) ledger.Coords {
	h.mu.Lock()
	defer h.mu.Unlock()
	return ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), h.coords)
}

// node is the calling node's (chat, node); ("", "") outside a node or when its thread is not registered.
func (s CallScope) node(ctx agent.Context) (chatID, nodeID string) {
	at, _ := s.advisorTask(ctx)
	return at.ChatID, at.NodeID
}
