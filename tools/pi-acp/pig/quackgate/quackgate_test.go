package quackgate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

type policyCase struct {
	Tool       string         `json:"tool"`
	Input      map[string]any `json:"input"`
	AllowClone bool           `json:"allowClone"`
}

type verdict struct {
	Block string `json:"block,omitempty"`
	Ask   string `json:"ask,omitempty"`
}

var policyCases = func() []policyCase {
	bash := func(cmd string, allowClone bool) policyCase {
		return policyCase{"bash", map[string]any{"command": cmd}, allowClone}
	}
	read := func(tool, path string) policyCase { return policyCase{tool, map[string]any{"path": path}, false} }
	var cs []policyCase
	for _, cmd := range []string{
		"git push origin main", "git push", "  git push origin", "git push  origin main", "git push\torigin",
		"git push\vorigin", "git push\u00a0origin", "git push\u3000origin", "git push\ufefforigin", "git push\u0085origin",
		"\u00a0git push", "\ufeffgit push", "git pushy", "git -C . push", "cd x && git push", "echo git push",
		"git clone --depth 1 https://x/y", "git clone", "gh repo clone x/y", "gh repo clone", "git cloned", "echo hi", "",
	} {
		cs = append(cs, bash(cmd, false), bash(cmd, true))
	}
	cs = append(cs, policyCase{"bash", map[string]any{}, false}, policyCase{"edit", map[string]any{"command": "git push"}, false})
	for _, p := range []string{".env", "app/.env", "app/.env.prod", "/abs/x.env", "a.env.b.c", ".environment", "env", "app/.envrc", ".env/x", ""} {
		cs = append(cs, read("read", p), read("write", p))
	}
	return cs
}()

// TestCheckPolicyParity runs every case through mcp-client.mjs checkPolicy (the TS gate's) and the Go port.
func TestCheckPolicyParity(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	mod, _ := filepath.Abs("../../mcp-client.mjs")
	script := `import { checkPolicy } from ` + strconv.Quote((&url.URL{Scheme: "file", Path: mod}).String()) + `;
let s = ""; for await (const c of process.stdin) s += c;
process.stdout.write(JSON.stringify(JSON.parse(s).map((c) => checkPolicy(c.tool, c.input, { allowClone: c.allowClone }) ?? {})));`
	in, _ := json.Marshal(policyCases)
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node checkPolicy: %v", err)
	}
	var js []verdict
	if err := json.Unmarshal(out, &js); err != nil || len(js) != len(policyCases) {
		t.Fatalf("node output %s: %v", out, err)
	}
	for i, c := range policyCases {
		block, ask := checkPolicy(c.Tool, c.Input, c.AllowClone)
		if got := (verdict{block, ask}); got != js[i] {
			t.Errorf("%s %q allowClone=%v: go %+v, js %+v", c.Tool, c.Input, c.AllowClone, got, js[i])
		}
	}
}

func TestCheckPolicyDeniesDelivery(t *testing.T) {
	for _, tc := range []struct {
		cmd        string
		allowClone bool
		denied     bool
	}{
		{"git push origin main", true, true},
		{"git clone https://x/y", false, true},
		{"git clone https://x/y", true, false},
		{"gh repo clone x/y", false, true},
		{"gh repo clone x/y", true, false},
		{"git status", false, false},
	} {
		block, _ := checkPolicy("bash", map[string]any{"command": tc.cmd}, tc.allowClone)
		if (block != "") != tc.denied {
			t.Errorf("%q allowClone=%v: block=%q", tc.cmd, tc.allowClone, block)
		}
	}
}

// permServer stands in for the shim's loopback /perm and /ready endpoints.
func permServer(t *testing.T, allow bool) (*bridgeCfg, *[]string) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]bool{"allow": allow})
	}))
	t.Cleanup(srv.Close)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	return &bridgeCfg{PermPort: port}, &hits
}

func TestGate(t *testing.T) {
	envRead := map[string]any{"path": "app/.env"}
	allowCfg, hits := permServer(t, true)
	if v := gate(allowCfg, "read", envRead); v != nil {
		t.Errorf("judge-allowed .env read blocked: %v", v)
	}
	if v := gate(allowCfg, "read", map[string]any{"path": "main.go"}); v != nil || len(*hits) != 1 {
		t.Errorf("plain read: %v, asks %v", v, *hits)
	}
	if v := gate(allowCfg, "bash", map[string]any{"command": "git push"}); v["block"] != true || len(*hits) != 1 {
		t.Errorf("git push: %v, asks %v", v, *hits)
	}
	rejectCfg, _ := permServer(t, false)
	if v := gate(rejectCfg, "read", envRead); v["reason"] != "denied by quack safety judge" {
		t.Errorf("judge-rejected .env read: %v", v)
	}
	if v := gate(&bridgeCfg{PermPort: 1}, "read", envRead); v["block"] != true {
		t.Errorf("unreachable judge must refuse: %v", v)
	}
	reportReady(allowCfg)
	if (*hits)[len(*hits)-1] != "/ready" {
		t.Errorf("ready ping hit %v", *hits)
	}
}

func TestLoadCfgFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadCfg(""); err == nil {
		t.Error("unset dir must not fall back to the cwd")
	}
	if _, err := loadCfg(dir); err == nil {
		t.Error("missing quackmcp.json accepted")
	}
	ext := filepath.Join(dir, "extensions")
	_ = os.MkdirAll(ext, 0o755)
	for body, ok := range map[string]bool{`{`: false, `{"prefix":"q"}`: false, `{"prefix":"q","permPort":9,"tools":[{"name":"t"}]}`: true} {
		_ = os.WriteFile(filepath.Join(ext, "quackmcp.json"), []byte(body), 0o644)
		if _, err := loadCfg(dir); (err == nil) != ok {
			t.Errorf("%s: err=%v", body, err)
		}
		t.Setenv("PIG_CODING_AGENT_DIR", dir)
		if Extension().Name() != "quackgate" {
			t.Error("factory name")
		}
	}
}

func TestMCPBridge(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		methods = append(methods, m.Method)
		w.Header().Set("mcp-session-id", "s1")
		if m.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(`event: message` + "\n" + `data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"staged"}],"isError":false}}` + "\n\n"))
	}))
	defer srv.Close()
	c := &mcpClient{url: srv.URL}
	r, err := c.call("stage_review", map[string]any{"verdict": "approve"})
	if tr, ok := r.(sdk.ToolResult); err != nil || !ok || tr.Content != "staged" || tr.IsError {
		t.Errorf("tools/call result %#v, err %v", r, err)
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/call" || c.session != "s1" {
		t.Errorf("methods %v session %q err %v", methods, c.session, err)
	}
	if _, err := decodeRPC("x", []byte(`{"error":{"message":"boom"}}`), "application/json"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("rpc error not surfaced: %v", err)
	}
	if _, err := decodeRPC("x", []byte(`nope`), "application/json"); err == nil {
		t.Error("garbage accepted")
	}
}
