package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gateReadyTimeout bounds how long session/new waits for quackgate's /ready ping; quack's own start timeout is 60s.
var gateReadyTimeout = 20 * time.Second

// config is PI_ACP_CONFIG, generated per agent by quack (serve.go piACPEnv).
type config struct {
	Endpoint          string   `json:"endpoint"`
	APIKey            string   `json:"api_key"`
	Model             string   `json:"model"`
	ContextWindow     int      `json:"context_window"`
	MaxOutputTokens   int      `json:"max_output_tokens"`
	SkillPaths        []string `json:"skill_paths"`
	AllowClone        bool     `json:"allow_clone"`
	OpenRouterRouting any      `json:"openrouter_routing"`
}

// shim is one ACP connection: at most one live session, backed by one engine process.
type shim struct {
	cfg       config
	stateRoot string
	engineCmd string

	connSet chan struct{}
	conn    *sdk.AgentSideConnection

	permOnce sync.Once
	permPort int
	permErr  error

	mu           sync.Mutex
	sessionID    sdk.SessionId
	eng          *engine
	gateReady    chan struct{}
	turn         *turn
	resumeFailed bool
}

func newShim(cfg config, stateRoot, engineCmd string) *shim {
	return &shim{cfg: cfg, stateRoot: stateRoot, engineCmd: engineCmd, connSet: make(chan struct{})}
}

// setConn publishes conn; the SDK serves stdin from inside NewAgentSideConnection, so a handler can run first.
func (s *shim) setConn(c *sdk.AgentSideConnection) {
	s.conn = c
	close(s.connSet)
}

func (s *shim) client() *sdk.AgentSideConnection {
	<-s.connSet
	return s.conn
}

// sessionDir is keyed by session id, not random, so a fresh shim on session/load finds what session/new wrote.
func (s *shim) sessionDir(id sdk.SessionId) string {
	return filepath.Join(s.stateRoot, "pi-acp-"+string(id))
}

// hasSession checks for the engine's "<timestamp>_<id>.jsonl" before spawning, so a stale id fails the load.
func (s *shim) hasSession(id sdk.SessionId) bool {
	entries, _ := os.ReadDir(filepath.Join(s.sessionDir(id), "pi-sessions"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_"+string(id)+".jsonl") {
			return true
		}
	}
	return false
}

func (s *shim) writeEngineConfig(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "extensions"), 0o755); err != nil {
		return err
	}
	if s.cfg.Model != "" {
		model := map[string]any{"id": s.cfg.Model}
		// Omitted rather than 0: the engine rejects contextWindow <= 0 and falls back to its own defaults anyway.
		if s.cfg.ContextWindow > 0 {
			model["contextWindow"] = s.cfg.ContextWindow
		}
		if s.cfg.MaxOutputTokens > 0 {
			model["maxTokens"] = s.cfg.MaxOutputTokens
		}
		compat := map[string]any{"supportsDeveloperRole": false, "supportsReasoningEffort": false}
		if s.cfg.OpenRouterRouting != nil {
			compat["openRouterRouting"] = s.cfg.OpenRouterRouting
		}
		provider := map[string]any{"baseUrl": s.cfg.Endpoint, "api": "openai-completions", "apiKey": cmp.Or(s.cfg.APIKey, "unused"), "compat": compat, "models": []any{model}}
		if err := writeJSON(filepath.Join(dir, "models.json"), map[string]any{"providers": map[string]any{"quack": provider}}); err != nil {
			return err
		}
	}
	if len(s.cfg.SkillPaths) > 0 {
		return writeJSON(filepath.Join(dir, "settings.json"), map[string]any{"skills": s.cfg.SkillPaths})
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// mcpServer picks quack's loopback MCP server out of session/new; it is the only one quack sends.
func mcpServer(servers []sdk.McpServer) (name, url string) {
	for _, m := range servers {
		switch {
		case m.Http != nil:
			return m.Http.Name, m.Http.Url
		case m.Sse != nil:
			return m.Sse.Name, m.Sse.Url
		}
	}
	return "", ""
}

// writeBridge lists quack's MCP tools and writes quackmcp.json for quackgate, returning the bridged tool names.
func (s *shim) writeBridge(ctx context.Context, dir string, servers []sdk.McpServer) (prefix string, names []string, err error) {
	name, url := mcpServer(servers)
	prefix = cmp.Or(name, "quackmcp")
	tools := []*mcp.Tool{}
	if url != "" {
		if tools, err = listTools(ctx, url); err != nil {
			return "", nil, fmt.Errorf("mcp bridge: %w", err)
		}
	}
	for _, t := range tools {
		names = append(names, prefix+"_"+t.Name)
	}
	cfg := map[string]any{"url": url, "prefix": prefix, "tools": tools, "permPort": s.permPort, "allowClone": s.cfg.AllowClone}
	return prefix, names, writeJSON(filepath.Join(dir, "extensions", "quackmcp.json"), cfg)
}

func listTools(ctx context.Context, url string) ([]*mcp.Tool, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "quack-acp", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	var tools []*mcp.Tool
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// startPerm starts the loopback endpoint quackgate posts /ready and approval-needed tool calls to.
func (s *shim) startPerm() error {
	s.permOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			s.permErr = err
			return
		}
		s.permPort = ln.Addr().(*net.TCPAddr).Port
		go func() { _ = http.Serve(ln, http.HandlerFunc(s.servePerm)) }()
	})
	return s.permErr
}

func (s *shim) servePerm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ready" {
		s.mu.Lock()
		if s.gateReady != nil {
			close(s.gateReady)
			s.gateReady = nil
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ask struct {
		ToolName string         `json:"toolName"`
		Title    string         `json:"title"`
		Input    map[string]any `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&ask)
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"allow": s.askPermission(r.Context(), ask.ToolName, ask.Title, ask.Input)})
}

// askPermission escalates to quack over session/request_permission, which routes it to the safety judge.
// Any failure denies.
func (s *shim) askPermission(ctx context.Context, tool, title string, input map[string]any) bool {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	resp, err := s.client().RequestPermission(ctx, sdk.RequestPermissionRequest{
		SessionId: sid,
		ToolCall: sdk.ToolCallUpdate{
			ToolCallId: sdk.ToolCallId(fmt.Sprintf("perm-%d", time.Now().UnixNano())),
			Title:      sdk.Ptr(title),
			Kind:       sdk.Ptr(nativeKind(tool)),
			RawInput:   input,
		},
		Options: []sdk.PermissionOption{
			{OptionId: "allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
			{OptionId: "reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
		},
	})
	return err == nil && resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow"
}

// startSession is the shared setup behind session/new and session/load: config, bridge, engine, gate ping.
func (s *shim) startSession(ctx context.Context, id sdk.SessionId, cwd string, servers []sdk.McpServer, resume bool) error {
	s.stopEngine()
	dir := s.sessionDir(id)
	if err := s.writeEngineConfig(dir); err != nil {
		return err
	}
	if err := s.startPerm(); err != nil {
		return err
	}
	prefix, bridged, err := s.writeBridge(ctx, dir, servers)
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	s.mu.Lock()
	s.sessionID, s.gateReady = id, ready
	s.mu.Unlock()
	eng, err := s.spawn(id, cwd, dir, prefix, bridged, resume)
	if err != nil {
		return err
	}
	// The safety-judge boundary: no session until the engine's gate proves it is subscribed.
	select {
	case <-ready:
	case <-eng.exited:
		return errors.New("engine exited before quackgate reported in")
	case <-time.After(gateReadyTimeout):
		eng.kill()
		return fmt.Errorf("quackgate did not report in within %s; refusing an ungated engine", gateReadyTimeout)
	}
	s.mu.Lock()
	s.eng = eng
	s.mu.Unlock()
	return nil
}

func (s *shim) stopEngine() {
	s.mu.Lock()
	eng := s.eng
	s.eng = nil
	s.mu.Unlock()
	if eng != nil {
		eng.kill()
	}
}

func (s *shim) liveEngine() *engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eng
}
