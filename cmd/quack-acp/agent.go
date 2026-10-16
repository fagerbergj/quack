package main

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
)

var (
	_ sdk.Agent                  = (*shim)(nil)
	_ sdk.AgentLoader            = (*shim)(nil)
	_ sdk.ExtensionMethodHandler = (*shim)(nil)
)

// rpcErr puts the reason in the JSON-RPC message; the SDK reports a plain error as "Internal error".
func rpcErr(msg string) error {
	return &sdk.RequestError{Code: -32000, Message: msg}
}

func (s *shim) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{
			LoadSession:     true,
			McpCapabilities: sdk.McpCapabilities{Http: true, Sse: true},
		},
		AuthMethods: []sdk.AuthMethod{},
	}, nil
}

func (s *shim) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	id := newSessionID()
	if err := s.startSession(ctx, id, p.Cwd, p.McpServers, false); err != nil {
		return sdk.NewSessionResponse{}, rpcErr(err.Error())
	}
	return sdk.NewSessionResponse{SessionId: id}, nil
}

// LoadSession resumes without replaying prior turns: quack waits only on this call's own response.
func (s *shim) LoadSession(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	if !s.hasSession(p.SessionId) {
		return sdk.LoadSessionResponse{}, rpcErr("no persisted session for " + string(p.SessionId))
	}
	if err := s.startSession(ctx, p.SessionId, p.Cwd, p.McpServers, true); err != nil {
		return sdk.LoadSessionResponse{}, rpcErr(err.Error())
	}
	return sdk.LoadSessionResponse{}, nil
}

// Prompt runs one engine turn to agent_settled; a cancel aborts the turn and still waits for that settle.
func (s *shim) Prompt(_ context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	eng := s.liveEngine()
	if eng == nil {
		return sdk.PromptResponse{}, rpcErr("no live session")
	}
	var text strings.Builder
	for _, b := range p.Prompt {
		if b.Text != nil {
			text.WriteString(b.Text.Text)
		}
	}
	t := &turn{done: make(chan struct{})}
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
	if err := eng.send(map[string]any{"type": "prompt", "message": text.String()}); err != nil {
		return sdk.PromptResponse{}, rpcErr("engine stdin: " + err.Error())
	}
	select {
	case <-t.done:
	case <-eng.exited:
		return sdk.PromptResponse{}, rpcErr("engine exited mid-prompt")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumeFailed {
		return sdk.PromptResponse{}, rpcErr("session/load did not actually resume - the engine started a blank session")
	}
	resp := sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Meta: t.lastStop}
	if t.cancelled {
		resp.StopReason = sdk.StopReasonCancelled
	}
	// A settle with no model call must say so, not look like a clean stop.
	if resp.Meta == nil {
		resp.Meta = map[string]any{"pi_stop_reason": "none"}
	}
	return resp, nil
}

func (s *shim) recordStop(stop map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil {
		s.turn.lastStop = stop
	}
}

func (s *shim) settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil {
		close(s.turn.done)
		s.turn = nil
	}
}

func (s *shim) Cancel(context.Context, sdk.CancelNotification) error {
	s.mu.Lock()
	if s.turn != nil {
		s.turn.cancelled = true
	}
	eng := s.eng
	s.mu.Unlock()
	if eng != nil {
		_ = eng.send(map[string]any{"type": "abort"})
	}
	return nil
}

// HandleExtensionMethod serves _quack/steer: a queued message forwarded into the engine's live turn.
func (s *shim) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != "_quack/steer" {
		return nil, sdk.NewMethodNotFound(method)
	}
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	eng, live := s.eng, s.turn != nil
	s.mu.Unlock()
	if eng == nil || !live {
		return nil, rpcErr("no live round to steer")
	}
	if err := eng.send(map[string]any{"type": "steer", "message": p.Text}); err != nil {
		return nil, rpcErr("engine stdin: " + err.Error())
	}
	return map[string]any{}, nil
}

func (s *shim) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, sdk.NewMethodNotFound("authenticate")
}

func (s *shim) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, sdk.NewMethodNotFound("logout")
}

func (s *shim) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, sdk.NewMethodNotFound("session/close")
}

func (s *shim) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, sdk.NewMethodNotFound("session/list")
}

func (s *shim) ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, sdk.NewMethodNotFound("session/resume")
}

func (s *shim) SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, sdk.NewMethodNotFound("session/set_config_option")
}

func (s *shim) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, sdk.NewMethodNotFound("session/set_mode")
}
