package dag

import (
	"context"
	"encoding/json"
	"fmt"

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
func buildSessionHandle(cfg vetting.Config, node Node, token string) SessionHandle {
	h := SessionHandle{Agent: node.AgentName, Scope: cfg.NodeID, HeadSHA: vetting.CloneHeadSHA(cfg)}
	if cfg.ExternalWorker {
		h.Kind = "acp"
		if t, ok := vetting.LookupAdvisorThread(token); ok {
			h.ID = t.ACPSessionID
		}
	} else {
		h.Kind = "adk"
		h.ID = cfg.ChatID
	}
	return h
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

// continuePreamble prefixes an ADK-native node's first-round prompt with the
// prior node's final answer, so the model has the conversation it's
// continuing - native agents have no protocol session to reload (unlike
// ACP's session/load), so the prior turn's content IS the resume mechanism.
func continuePreamble(c continuation) string {
	if !c.ok || c.handle.Kind != "adk" || c.priorOutput == "" {
		return ""
	}
	return "--- Continuing your own prior work on this (from an earlier turn) ---\n\n" +
		c.priorOutput + "\n\n--- End prior work; continue from here ---\n\n"
}
