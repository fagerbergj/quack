// Package quackgate is quack's tool_call policy gate and quackmcp_* bridge on PIG's Go SDK. PIG builds it into
// the pig-quack Piglet (../quack.yaml); `go test` here covers the gate logic.
package quackgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

type bridgeCfg struct {
	URL        string `json:"url"`
	Prefix     string `json:"prefix"`
	PermPort   int    `json:"permPort"`
	AllowClone bool   `json:"allowClone"`
	Tools      []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	} `json:"tools"`
}

// jsSpace is JavaScript's \s: unlike Go's \s it takes Unicode spaces, so "git push\u00a0origin" still matches.
const jsSpace = `[\s\v\pZ\x{feff}]`

var (
	deny    = []*regexp.Regexp{regexp.MustCompile(`^git push(` + jsSpace + `|$)`)}
	clone   = []*regexp.Regexp{regexp.MustCompile(`^git clone(` + jsSpace + `|$)`), regexp.MustCompile(`^gh repo clone(` + jsSpace + `|$)`)}
	envFile = regexp.MustCompile(`(^|/)[^/]*\.env(\.[^/]*)?$`)
	jsTrim  = regexp.MustCompile(`^` + jsSpace + `+|` + jsSpace + `+$`)
)

// checkPolicy returns block, ask, or both "" for allow. The deny matches only a leading command, a nudge not a
// boundary: quack strips the child's git credentials. An ask escalates to quack's safety judge via the shim.
func checkPolicy(tool string, input map[string]any, allowClone bool) (block, ask string) {
	if tool == "bash" {
		cmd, _ := input["command"].(string)
		cmd = jsTrim.ReplaceAllString(cmd, "")
		res := deny
		if !allowClone {
			res = append(append([]*regexp.Regexp{}, deny...), clone...)
		}
		for _, re := range res {
			if re.MatchString(cmd) {
				f := strings.Split(cmd, " ")
				return "denied by policy: " + strings.Join(f[:min(3, len(f))], " ") + " (delivery is gate-owned)", ""
			}
		}
	}
	if p, _ := input["path"].(string); tool == "read" && envFile.MatchString(p) {
		return "", "read " + p
	}
	return "", ""
}

// loadCfg reads the shim's quackmcp.json. An unset dir is an error, not the cwd: the work tree is not trusted.
func loadCfg(dir string) (*bridgeCfg, error) {
	if dir == "" {
		return nil, errors.New("PIG_CODING_AGENT_DIR is unset")
	}
	b, err := os.ReadFile(filepath.Join(dir, "extensions", "quackmcp.json"))
	if err != nil {
		return nil, err
	}
	var c bridgeCfg
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.PermPort <= 0 {
		return nil, errors.New("quackmcp.json has no permPort")
	}
	return &c, nil
}

func post(url string, body any, hdr map[string]string) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// gate is the tool_call verdict: nil allows, a block map refuses. Any failure to reach the judge refuses.
func gate(cfg *bridgeCfg, name string, input map[string]any) map[string]any {
	block, ask := checkPolicy(name, input, cfg.AllowClone)
	if block != "" {
		return map[string]any{"block": true, "reason": block}
	}
	if ask == "" {
		return nil
	}
	res, err := post(fmt.Sprintf("http://127.0.0.1:%d/perm", cfg.PermPort), map[string]any{"toolName": name, "title": ask, "input": input}, nil)
	var r struct{ Allow bool }
	if err == nil {
		_ = json.NewDecoder(res.Body).Decode(&r)
		_ = res.Body.Close()
	}
	if !r.Allow {
		return map[string]any{"block": true, "reason": "denied by quack safety judge"}
	}
	return nil
}

// reportReady tells the shim the gate is subscribed; the shim holds session/new until it hears this.
func reportReady(cfg *bridgeCfg) {
	res, err := post(fmt.Sprintf("http://127.0.0.1:%d/ready", cfg.PermPort), map[string]any{"gate": "quackgate"}, nil)
	if err == nil {
		_ = res.Body.Close()
	}
}

// mcpClient is a minimal streamable-HTTP MCP client.
type mcpClient struct {
	url, session string
	once         sync.Once
	id           int
	mu           sync.Mutex
}

func (m *mcpClient) rpc(method string, params any, notify bool) (map[string]any, error) {
	m.mu.Lock()
	m.id++
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notify {
		msg["id"] = m.id
	}
	hdr := map[string]string{}
	if m.session != "" {
		hdr["mcp-session-id"] = m.session
	}
	m.mu.Unlock()
	res, err := post(m.url, msg, hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if s := res.Header.Get("mcp-session-id"); s != "" {
		m.mu.Lock()
		m.session = s
		m.mu.Unlock()
	}
	if notify || res.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	raw, _ := io.ReadAll(res.Body)
	return decodeRPC(method, raw, res.Header.Get("content-type"))
}

// decodeRPC takes the first data: line of an SSE body.
func decodeRPC(method string, raw []byte, contentType string) (map[string]any, error) {
	if strings.Contains(contentType, "text/event-stream") {
		for l := range strings.SplitSeq(string(raw), "\n") {
			if d, ok := strings.CutPrefix(l, "data:"); ok {
				raw = []byte(strings.TrimSpace(d))
				break
			}
		}
	}
	var out struct {
		Result map[string]any            `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

func (m *mcpClient) connect() {
	m.once.Do(func() {
		_, _ = m.rpc("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "quackgate", "version": "0"}}, false)
		_, _ = m.rpc("notifications/initialized", map[string]any{}, true)
	})
}

func (m *mcpClient) call(name string, params map[string]any) (any, error) {
	m.connect()
	r, err := m.rpc("tools/call", map[string]any{"name": name, "arguments": params}, false)
	if err != nil {
		return nil, err
	}
	var txt strings.Builder
	if cs, ok := r["content"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok && m["type"] == "text" {
				txt.WriteString(fmt.Sprint(m["text"]))
			}
		}
	}
	isErr, _ := r["isError"].(bool)
	return sdk.ToolResult{Content: txt.String(), IsError: isErr}, nil
}

// toolCallHandler is the tool_call verdict; a nil cfg refuses every call.
func toolCallHandler(cfg *bridgeCfg) sdk.EventFunc {
	if cfg == nil {
		return func(sdk.Context, map[string]any) (any, error) {
			return map[string]any{"block": true, "reason": "quackgate has no config: tool calls are refused"}, nil
		}
	}
	return func(_ sdk.Context, data map[string]any) (any, error) {
		name, _ := data["toolName"].(string)
		input, _ := data["input"].(map[string]any)
		if v := gate(cfg, name, input); v != nil {
			return v, nil
		}
		return nil, nil
	}
}

// Extension is the factory pig-quack fuses in. Without a valid config it blocks every tool call and never
// reports ready, so the shim refuses the session too.
func Extension() *sdk.Extension {
	e := sdk.New("quackgate")
	cfg, err := loadCfg(os.Getenv("PIG_CODING_AGENT_DIR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "quackgate: no usable quackmcp.json, blocking every tool call:", err)
	}
	e.OnEvent(sdk.EventToolCall, toolCallHandler(cfg))
	if cfg == nil {
		return e
	}
	// session_start, not the factory: the ping then proves the engine dispatches events to this extension.
	e.OnEvent(sdk.EventSessionStart, func(sdk.Context, map[string]any) (any, error) {
		reportReady(cfg)
		return nil, nil
	})
	client := &mcpClient{url: cfg.URL}
	for _, t := range cfg.Tools {
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		// The SDK panics on a non-object schema; an MCP tool may legally omit one.
		if t.InputSchema == nil {
			t.InputSchema = map[string]any{"type": "object"}
		}
		e.Tool(cfg.Prefix+"_"+t.Name, desc, t.InputSchema, func(_ sdk.Context, params map[string]any) (any, error) {
			return client.call(t.Name, params)
		})
	}
	return e
}
