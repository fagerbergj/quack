package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// buildExtChild compiles the real quack-ext-<name> binary, as the Dockerfile ships it.
func buildExtChild(t *testing.T, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "quack-ext-"+name)
	cmd := exec.Command("go", "build", "-o", bin, "github.com/fagerbergj/quack/extensions/"+name+"/cmd/quack-ext-"+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build quack-ext-%s: %v\n%s", name, err, out)
	}
	return bin
}

type usageReply struct {
	status   int
	location string
	body     string
}

// serveUsage boots extensions.usage from block and returns what each path answers through quack's router.
func serveUsage(t *testing.T, block string, paths []string) (map[string]usageReply, *builtSDKExtension) {
	t.Helper()
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	var judgeModelRef atomic.Pointer[model.LLM]
	cfg := noopModulesConfig(t, t.TempDir(), block)
	exts, err := buildSDKExtensions(cfg, nil, nil, nil, &orchRef, nil, nil, &judgeModelRef, nil, nil, nil, nil, nil)
	if err != nil || len(exts) != 1 {
		t.Fatalf("buildSDKExtensions = %d exts, %v", len(exts), err)
	}
	t.Cleanup(func() { closeSDKExtensions(exts) })
	ts := httptest.NewServer(server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(exts)}))
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	out := map[string]usageReply{}
	for _, p := range paths {
		resp, err := client.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		out[p] = usageReply{resp.StatusCode, resp.Header.Get("Location"), string(body)}
	}
	return out, &exts[0]
}

// TestUsageOutOfProcessMatchesInProcess: with exec set, quack spawns quack-ext-usage and every usage
// route answers exactly as the compiled-in extension does, Prometheus proxying included.
func TestUsageOutOfProcessMatchesInProcess(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","path":"`+r.URL.Path+`","query":"`+r.URL.Query().Get("query")+`"}`)
	}))
	defer prom.Close()
	bin := buildExtChild(t, "usage")
	block := "usage:\n  prometheus_url: " + prom.URL + "\n  tempo_url: http://tempo:3200\n"
	paths := []string{"/usage", "/usage/", "/usage/app.js", "/usage/api/query?query=up&evil=1", "/usage/api/query", "/usage/nope"}

	want, _ := serveUsage(t, block, paths)
	got, ext := serveUsage(t, block+"  exec: "+bin+"\n", paths)

	if ext.title != "Usage" || ext.href != "/usage/" {
		t.Errorf("UI descriptor = %q %q, want the child's Usage /usage/", ext.title, ext.href)
	}
	if r := got["/usage/api/query?query=up&evil=1"]; r.status != http.StatusOK || !strings.Contains(r.body, `"path":"/api/v1/query","query":"up"`) {
		t.Errorf("proxied query = %+v, want Prometheus' answer", r)
	}
	for _, p := range paths {
		if got[p] != want[p] {
			t.Errorf("GET %s: out of process %+v, in process %+v", p, got[p], want[p])
		}
	}
}

func TestValidateExtensionsRejectsMissingExec(t *testing.T) {
	cfg := noopModulesConfig(t, t.TempDir(), "usage:\n  prometheus_url: http://prom:9090\n  exec: /nonexistent/quack-ext-usage\n")
	if _, err := ValidateExtensions(cfg); err == nil || !strings.Contains(err.Error(), "extensions.usage: exec") {
		t.Fatalf("ValidateExtensions = %v, want an exec error", err)
	}
}
