package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// PriorNode is what a store lookup returns about an earlier node the
// continue primitive might resume - see NodeLookup.
type PriorNode struct {
	Status NodeStatus
	Handle SessionHandle
	Output string // full vetted text; seeds an ADK node's continuation prompt
}

// NodeLookup resolves node.Continue against the chat's persisted node
// history (any plan, not just the one being built - a follow-up's plan is
// saved before the executor runs, so "latest plan" would be this one).
// Wired to store.FindDagNodeByChat: dag cannot import store, since store
// already imports dag.
type NodeLookup func(ctx context.Context, chatID, nodeID, excludePlanID string) (PriorNode, bool, error)

// continuation is resolveContinue's verdict for one node's Continue field.
type continuation struct {
	ok             bool
	handle         SessionHandle
	priorOutput    string
	fallbackReason string // "" when ok, or when the node declared no Continue at all
}

// resolveContinue validates node.Continue's eligibility (same agent, same
// workspace scope, prior node terminal, fresh HEAD) per the design: on any
// failure the node starts fresh and fallbackReason says why (logged, never
// surfaced as a user-facing error - a continue: is a hint, not a contract).
func resolveContinue(ctx context.Context, node Node, cfg vetting.Config, lookup NodeLookup, chatID, excludePlanID string) continuation {
	if node.Continue == "" {
		return continuation{}
	}
	if lookup == nil {
		return continuation{fallbackReason: fmt.Sprintf("continue %q: no node-lookup wired", node.Continue)}
	}
	prior, found, err := lookup(ctx, chatID, node.Continue, excludePlanID)
	if err != nil {
		return continuation{fallbackReason: fmt.Sprintf("continue %q: lookup failed: %v", node.Continue, err)}
	}
	if !found {
		return continuation{fallbackReason: fmt.Sprintf("continue %q: no prior node state found", node.Continue)}
	}
	switch prior.Status {
	case StatusDone, StatusFailed, StatusCancelled:
	default:
		return continuation{fallbackReason: fmt.Sprintf("continue %q: prior node is %s, not terminal", node.Continue, prior.Status)}
	}
	if prior.Handle.Agent != node.AgentName {
		return continuation{fallbackReason: fmt.Sprintf("continue %q: prior node ran agent %q, this node is %q", node.Continue, prior.Handle.Agent, node.AgentName)}
	}
	// Freshness: recompute the shared clone's HEAD at the prior node's own
	// scope - a push landing between turns must not resume onto a tree the
	// prior session never saw.
	checkCfg := cfg
	checkCfg.NodeID = prior.Handle.Scope
	if live := vetting.CloneHeadSHA(checkCfg); live != prior.Handle.HeadSHA {
		return continuation{fallbackReason: fmt.Sprintf("continue %q: workspace HEAD moved (%s -> %s)", node.Continue, short12(prior.Handle.HeadSHA), short12(live))}
	}
	return continuation{ok: true, handle: prior.Handle, priorOutput: prior.Output}
}

func short12(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// buildSessionHandle captures the durable handle a just-finished node leaves
// behind for a future continue: - the ADK session id (native agents, always
// the chat's own session) or the ACP protocol session id (external agents,
// read back from the advisor thread since round() only updates it there).
// ctx is the node's OWN activation context (post-override if this node
// itself was a continuation), so Branch/IsolationScope chain forward.
// setup/delivery are the plan's own (possibly nil) - stamped onto the handle
// so a later plan that continues this node can inherit them.
func buildSessionHandle(ctx adkagent.Context, cfg vetting.Config, node Node, setup *Setup, delivery *Delivery, token string) SessionHandle {
	h := SessionHandle{Agent: node.AgentName, Scope: cfg.NodeID, HeadSHA: vetting.CloneHeadSHA(cfg)}
	if setup != nil {
		h.Repo, h.BaseRef, h.WorkBranch = setup.Repo, setup.BaseRef, setup.WorkBranch
	}
	if delivery != nil {
		h.DeliveryKind = delivery.Kind
	}
	if cfg.ExternalWorker {
		h.Kind = "acp"
		if t, ok := vetting.LookupAdvisorThread(token); ok {
			h.ID = t.ACPSessionID
		}
	} else {
		h.Kind = "adk"
		h.ID = cfg.ChatID
		h.Branch = ctx.Branch()
		h.IsolationScope = ctx.IsolationScope()
	}
	return h
}

// resumeADKContext derives a context that runs under the prior node's own
// branch/isolation scope, so ADK's own history replay (internal/llminternal's
// ContentsRequestProcessor: branch prefix match + isolation-scope exact
// match) includes what the prior node said - not a text splice glued onto
// this node's prompt. "" branch is left alone (a legacy/never-captured
// handle): overriding to "" would mean universal visibility, not isolation.
func resumeADKContext(ctx adkagent.Context, c continuation) adkagent.Context {
	if !c.ok || c.handle.Kind != "adk" || c.handle.Branch == "" {
		return ctx
	}
	branch, scope := c.handle.Branch, c.handle.IsolationScope
	return ctx.WithDelta(&adkagent.CommonContextDelta{
		InvocationContextDelta: &adkagent.InvocationContextDelta{Branch: &branch, IsolationScope: &scope},
	})
}

// emitPriorAnswer injects the prior node's final answer as a genuine
// "model" turn on the resumed branch/isolation scope, authored as the SAME
// agent (resolveContinue already required an agent match) so ADK's contents
// processor keeps it as the agent's own prior turn rather than converting it
// to a synthetic "for context: X said" user turn (see ConvertForeignEvent).
// ADK's own per-round isolation scoping (workflow.WithIsolationScopeFromNodePath,
// see vetting.runWorkerNode) otherwise hides even same-branch events from a
// different node's rounds - ONLY this node's OWN rounds (via cfg.ResumedFrom,
// which makes runWorkerNode inherit this scope instead) end up scoped to
// match what's emitted here.
func emitPriorAnswer(ctx adkagent.Context, emit func(*session.Event) error, workerName string, c continuation) {
	if !c.ok || c.handle.Kind != "adk" || c.handle.Branch == "" || c.priorOutput == "" || emit == nil {
		return
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Author = workerName
	ev.Branch = c.handle.Branch
	ev.IsolationScope = c.handle.IsolationScope
	ev.LLMResponse.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: c.priorOutput}}}
	ev.LLMResponse.FinishReason = genai.FinishReasonStop
	if err := emit(ev); err != nil {
		slog.Warn("continue: prior-answer event emit failed; node resumes with no prior context", "component", "dag", "err", err)
	}
}

// EncodeSessionHandle/DecodeSessionHandle round-trip a handle through the
// flat string fields stream events and the DagNode store row carry it in -
// stream cannot import dag (dag already imports stream), so the wire shape
// there is a plain JSON string, not this struct.
func EncodeSessionHandle(h SessionHandle) string {
	if h == (SessionHandle{}) {
		return ""
	}
	b, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(b)
}

func DecodeSessionHandle(raw string) SessionHandle {
	if raw == "" {
		return SessionHandle{}
	}
	var h SessionHandle
	_ = json.Unmarshal([]byte(raw), &h)
	return h
}
