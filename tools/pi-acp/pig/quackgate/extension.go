// Package quackgate ports pi-acp.mjs's generated quackmcp.ts (tool_call gate + quackmcp_* bridge) to PIG's Go SDK.
// Spike: PIG builds it (`pig -e` or `pig piglet build ../quack.yaml`), not `go build`; go.mod relies on PIG's staged SDK.
package quackgate

import (
	"bytes"
	"encoding/json"
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

var (
	deny    = []*regexp.Regexp{regexp.MustCompile(`^git push(\s|$)`)}
	clone   = []*regexp.Regexp{regexp.MustCompile(`^git clone(\s|$)`), regexp.MustCompile(`^gh repo clone(\s|$)`)}
	envFile = regexp.MustCompile(`(^|/)[^/]*\.env(\.[^/]*)?$`)
)

// checkPolicy mirrors tools/pi-acp/mcp-client.mjs checkPolicy: block, ask, or "" for allow.
func checkPolicy(tool string, input map[string]any, allowClone bool) (block, ask string) {
	if tool == "bash" {
		cmd, _ := input["command"].(string)
		cmd = strings.TrimSpace(cmd)
		res := deny
		if !allowClone {
			res = append(append([]*regexp.Regexp{}, deny...), clone...)
		}
		for _, re := range res {
			if re.MatchString(cmd) {
				f := strings.Fields(cmd)
				return "denied by policy: " + strings.Join(f[:min(3, len(f))], " ") + " (delivery is gate-owned)", ""
			}
		}
	}
	if p, _ := input["path"].(string); tool == "read" && envFile.MatchString(p) {
		return "", "read " + p
	}
	return "", ""
}

func loadCfg() *bridgeCfg {
	dir := os.Getenv("PIG_CODING_AGENT_DIR")
	b, err := os.ReadFile(filepath.Join(dir, "extensions", "quackmcp.json"))
	if err != nil {
		return nil
	}
	var c bridgeCfg
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

func post(url string, body any, hdr map[string]string) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// mcpClient is the minimal streamable-HTTP client mcp-client.mjs implements.
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
	defer res.Body.Close()
	if s := res.Header.Get("mcp-session-id"); s != "" {
		m.session = s
	}
	if notify || res.StatusCode == 202 {
		return nil, nil
	}
	raw, _ := io.ReadAll(res.Body)
	if strings.Contains(res.Header.Get("content-type"), "text/event-stream") {
		for _, l := range strings.Split(string(raw), "\n") {
			if d, ok := strings.CutPrefix(l, "data:"); ok {
				raw = []byte(strings.TrimSpace(d))
				break
			}
		}
	}
	var out struct {
		Result map[string]any `json:"result"`
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

func Extension() *sdk.Extension {
	e := sdk.New("quackgate")
	cfg := loadCfg()
	if cfg == nil {
		// Spike only: a real gate must fail closed here, as the TS version does on a missing config.
		fmt.Fprintln(os.Stderr, "quackgate: no quackmcp.json under PIG_CODING_AGENT_DIR="+os.Getenv("PIG_CODING_AGENT_DIR")+"; gate NOT installed")
		return e
	}
	e.OnEvent("tool_call", func(_ sdk.Context, data map[string]any) (any, error) {
		name, _ := data["toolName"].(string)
		input, _ := data["input"].(map[string]any)
		block, ask := checkPolicy(name, input, cfg.AllowClone)
		if block != "" {
			return map[string]any{"block": true, "reason": block}, nil
		}
		if ask == "" {
			return nil, nil
		}
		res, err := post(fmt.Sprintf("http://127.0.0.1:%d/perm", cfg.PermPort), map[string]any{"toolName": name, "title": ask, "input": input}, nil)
		allow := false
		if err == nil {
			var r struct{ Allow bool }
			_ = json.NewDecoder(res.Body).Decode(&r)
			res.Body.Close()
			allow = r.Allow
		}
		if !allow {
			return map[string]any{"block": true, "reason": "denied by quack safety judge"}, nil
		}
		return nil, nil
	})
	client := &mcpClient{url: cfg.URL}
	for _, t := range cfg.Tools {
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		e.Tool(cfg.Prefix+"_"+t.Name, desc, t.InputSchema, func(_ sdk.Context, params map[string]any) (any, error) {
			client.connect()
			r, err := client.rpc("tools/call", map[string]any{"name": t.Name, "arguments": params}, false)
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
		})
	}
	return e
}
