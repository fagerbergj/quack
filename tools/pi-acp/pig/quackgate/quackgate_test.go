package quackgate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestCheckPolicy(t *testing.T) {
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	path := func(p string) map[string]any { return map[string]any{"path": p} }
	for _, tc := range []struct {
		tool       string
		input      map[string]any
		allowClone bool
		block, ask bool
	}{
		{"bash", bash("git push origin main"), true, true, false},
		{"bash", bash("git push"), false, true, false},
		{"bash", bash("  git push origin"), false, true, false},
		{"bash", bash("git push\torigin"), false, true, false},
		{"bash", bash("git push\vorigin"), false, true, false},
		{"bash", bash("\u00a0git push\u3000origin"), false, true, false},
		{"bash", bash("git push\ufefforigin"), false, true, false},
		{"bash", bash("\ufeffgit push"), false, true, false},
		// NEL is not JavaScript whitespace, so it does not separate the command.
		{"bash", bash("git push\u0085origin"), false, false, false},
		{"bash", bash("git pushy"), false, false, false},
		// The deny matches a leading command only: these get past it by design.
		{"bash", bash("git -C . push"), false, false, false},
		{"bash", bash("cd x && git push"), false, false, false},
		{"bash", bash("echo git push"), false, false, false},
		{"bash", bash("git clone https://x/y"), false, true, false},
		{"bash", bash("git clone https://x/y"), true, false, false},
		{"bash", bash("gh repo clone x/y"), false, true, false},
		{"bash", bash("gh repo clone x/y"), true, false, false},
		{"bash", bash("git cloned"), false, false, false},
		{"bash", bash(""), false, false, false},
		{"bash", map[string]any{}, false, false, false},
		{"edit", bash("git push"), false, false, false},
		{"read", path(".env"), false, false, true},
		{"read", path("app/.env"), false, false, true},
		{"read", path("app/.env.prod"), false, false, true},
		{"read", path("/abs/x.env"), false, false, true},
		{"read", path("a.env.b.c"), false, false, true},
		{"read", path(".env/x"), false, false, false},
		{"read", path(".environment"), false, false, false},
		{"read", path("app/.envrc"), false, false, false},
		{"read", path(""), false, false, false},
		{"write", path(".env"), false, false, false},
	} {
		block, ask := checkPolicy(tc.tool, tc.input, tc.allowClone)
		if (block != "") != tc.block || (ask != "") != tc.ask {
			t.Errorf("%s %q allowClone=%v: block=%q ask=%q", tc.tool, tc.input, tc.allowClone, block, ask)
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
	// The handler pig-quack calls routes through gate: a configured gate blocks git push, passes a plain read.
	h := toolCallHandler(allowCfg)
	if v, _ := h(sdk.Context{}, map[string]any{"toolName": "bash", "input": map[string]any{"command": "git push"}}); v.(map[string]any)["block"] != true {
		t.Errorf("handler let git push through: %v", v)
	}
	if v, _ := h(sdk.Context{}, map[string]any{"toolName": "read", "input": map[string]any{"path": "main.go"}}); v != nil {
		t.Errorf("handler blocked a plain read: %v", v)
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
	v, _ := toolCallHandler(nil)(sdk.Context{}, map[string]any{"toolName": "read", "input": map[string]any{"path": "main.go"}})
	if v.(map[string]any)["block"] != true {
		t.Errorf("unconfigured gate allowed a plain read: %v", v)
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
