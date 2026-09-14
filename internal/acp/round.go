package acp

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
)

// registerRoundAbort: the round's direct cancel line (#1030). Returns the
// matching unregister (a no-op when no hooks are wired or no steer target).
func (a *Agent) registerRoundAbort(steerChatID, steerNodeID string, abortCancel context.CancelFunc) func() {
	if a.opts.RegisterRoundAbort != nil && steerChatID != "" && steerNodeID != "" {
		a.opts.RegisterRoundAbort(steerChatID, steerNodeID, abortCancel)
		if a.opts.UnregisterRoundAbort != nil {
			return func() { a.opts.UnregisterRoundAbort(steerChatID, steerNodeID) }
		}
	}
	return func() {}
}

// steerHooks: live-steer registration and the preamble prepend (skipped for a
// live pinned process - a resumed session may have missed a preamble change).
func (a *Agent) steerHooks(h *procHandle, outbound, steerChatID, steerNodeID string, fromPinned bool) (string, func()) {
	unreg := func() {}
	// Live only for this round's duration; CallExtension (an acked request), not NotifyExtension: between the shim settling and the deferred Unregister the connection is still open, so a fire-and-forget notify would report delivered while the shim silently drops it (promptReq already nil).
	// A failed/errored call reports false, and enqueue's caller parks it instead (#998 review).
	if a.opts.RegisterLiveSteer != nil && steerChatID != "" && steerNodeID != "" {
		a.opts.RegisterLiveSteer(steerChatID, steerNodeID, steerForward(h.conn))
		if a.opts.UnregisterLiveSteer != nil {
			unreg = func() { a.opts.UnregisterLiveSteer(steerChatID, steerNodeID) }
		}
	}
	if a.opts.Preamble != "" && !fromPinned {
		outbound = a.opts.Preamble + "\n\n" + outbound
	}
	return outbound, unreg
}

// handshake: Initialize + session/load|new on a fresh process; the resume only
// ever matters on a node's FIRST round (pinned processes are the path after it).
func (a *Agent) handshake(ctx context.Context, cwd, memSecret, advisorToken, priorSessionID string, h *procHandle) (sessID sdk.SessionId, toolNames []string, resumed bool, err error) {
	ictx, cancelInit := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancelInit()
	handshakeCtx, handshakeSpan := otelobs.Start(ctx, "acp.handshake", attribute.String(otelobs.GenAIAgentName, a.name))
	_ = handshakeCtx
	var initResp sdk.InitializeResponse
	initResp, err = h.conn.Initialize(ictx, sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{},
	})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return "", nil, false, fmt.Errorf("acp: initialize: %w%s", err, h.stderrTail())
	}
	mcpServers := memoryMCPServers(memSecret, initResp.AgentCapabilities)
	memSession, _ := vetting.LookupMemSession(memSecret)
	toolNames = mcpToolNames(memSession, len(mcpServers) > 0)
	a.log.Info("acp negotiated capabilities", "mcp_http", initResp.AgentCapabilities.McpCapabilities.Http,
		"mcp_sse", initResp.AgentCapabilities.McpCapabilities.Sse, "mcp_acp", initResp.AgentCapabilities.McpCapabilities.Acp,
		"mcp_surface_offered", len(mcpServers) > 0, "has_mem_secret", memSecret != "", "mcp_tools", toolNames)
	sessID = sdk.SessionId(priorSessionID)
	if priorSessionID != "" && initResp.AgentCapabilities.LoadSession {
		_, err = h.conn.LoadSession(ictx, sdk.LoadSessionRequest{Cwd: cwd, McpServers: mcpServers, SessionId: sessID})
		resumed = err == nil
		if err != nil {
			a.log.Warn("acp session/load failed, starting a new session", "session", priorSessionID, "err", err)
		}
	}
	if priorSessionID == "" || !initResp.AgentCapabilities.LoadSession || err != nil {
		var sess sdk.NewSessionResponse
		sess, err = h.conn.NewSession(ictx, sdk.NewSessionRequest{Cwd: cwd, McpServers: mcpServers})
		if err != nil {
			otelobs.End(handshakeSpan, err)
			return "", nil, false, fmt.Errorf("acp: session/new: %w%s", err, h.stderrTail())
		}
		sessID = sess.SessionId
	}
	if advisorToken != "" {
		vetting.SetAdvisorThreadSessionID(advisorToken, string(sessID))
	}
	handshakeSpan.SetAttributes(attribute.String("session_id", string(sessID)))
	otelobs.End(handshakeSpan, nil)
	a.log.Info("acp round started", "cwd", cwd, "session", sessID, "resumed", priorSessionID != "" && sessID == sdk.SessionId(priorSessionID))
	return sessID, toolNames, resumed, nil
}

// relayDrain: emit every buffered wire update through relay; false when the
// caller stopped consuming (relay returned false).
func (a *Agent) relayDrain(h *procHandle, relay func(sdk.SessionUpdate) bool) bool {
	for _, u := range h.drainUpdates() {
		if !relay(u) {
			return false
		}
	}
	return true
}

// handlePromptDone: the round's terminal Prompt response - usage, refusal, final answer event.
// ctx wins per field, the shared stamp only fills blanks (#1048); returns whether the round pinned cleanly.
func (a *Agent) handlePromptDone(d promptDone, h *procHandle, tr *translator, endPrompt func(error), promptSpan oteltrace.Span, ctx context.Context, coords ledger.Coords, emit func(eventSpec) bool) (bool, error) {
	if d.err != nil {
		endPrompt(d.err)
		return false, fmt.Errorf("acp: prompt: %w%s", d.err, h.stderrTail())
	}
	recordUsage(a.opts.ModelName, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), coords), a.opts.Pricing, d.resp.Usage)
	if d.resp.StopReason == sdk.StopReasonRefusal {
		refusalErr := errors.New("acp: agent refused the prompt")
		endPrompt(refusalErr)
		return false, refusalErr
	}
	final := finalSpec(tr)
	a.log.Info("acp round done", "stop", string(d.resp.StopReason), "answer_len", len(final.parts[0].Text))
	promptSpan.SetAttributes(attribute.StringSlice(otelobs.GenAIResponseFinishReasons, []string{string(d.resp.StopReason)}))
	endPrompt(nil)
	emit(final)
	return true, nil
}

// roundLoopArgs: one round's event relay - everything the until-done loop
// reads besides the agent itself.
type roundLoopArgs struct {
	h          *procHandle
	sessID     sdk.SessionId
	done       chan promptDone
	tr         *translator
	turns      *turnSpans
	endPrompt  func(error)
	promptSpan oteltrace.Span
	idleTimer  idleTimer
	ctx        context.Context
	abortCtx   context.Context
	coords     ledger.Coords
	emit       func(eventSpec) bool
}

// roundLoop: relay wire updates and steer traffic until the Prompt RPC lands,
// a cancel/interrupt stops the round, or the agent wedges (idle). Returns whether the round pinned cleanly.
func (a *Agent) roundLoop(al *roundLoopArgs) (bool, error) {
	resetIdle := func() {
		if !al.idleTimer.Stop() {
			select {
			case <-al.idleTimer.C():
			default:
			}
		}
		al.idleTimer.Reset(a.opts.IdleTimeout)
	}
	relay := func(u sdk.SessionUpdate) bool {
		al.turns.observe(u)
		for _, spec := range al.tr.translate(u) {
			if !al.emit(spec) {
				return false
			}
		}
		return true
	}
	for {
		select {
		case <-al.h.notify:
			resetIdle()
			if !a.relayDrain(al.h, relay) {
				return false, nil
			}
		case d := <-al.done:
			if !a.relayDrain(al.h, relay) {
				return false, nil
			}
			return a.handlePromptDone(d, al.h, al.tr, al.endPrompt, al.promptSpan, al.ctx, al.coords, al.emit)
		case <-al.ctx.Done():
			a.gracefulCancel(al.h, al.sessID, al.done)
			return false, al.ctx.Err()
		case <-al.abortCtx.Done():
			a.gracefulCancel(al.h, al.sessID, al.done)
			return false, al.abortCtx.Err()
		case <-al.idleTimer.C():
			a.gracefulCancel(al.h, al.sessID, al.done)
			return false, fmt.Errorf("acp: no activity for %s - treating the ACP agent as wedged%s", a.opts.IdleTimeout, al.h.stderrTail())
		}
	}
}
