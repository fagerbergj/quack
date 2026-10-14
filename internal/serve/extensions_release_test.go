package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// exec: release refuses a mismatched checksum (boot fails; a restart 502s, old child serving), runs the pinned binary,
// and a re-pin plus restart runs the new one. Each fake release script marks its version, then execs quack-ext-usage.
func TestExecReleaseRunsThePinnedBinaryAndRestartsOntoANewPin(t *testing.T) {
	bin, marks := buildExtChild(t, "usage"), t.TempDir()
	asset := fmt.Sprintf("quack-ext-usage-%s-%s", runtime.GOOS, runtime.GOARCH)
	files := map[string][]byte{}
	for _, v := range []string{"v1", "v2", "v3"} {
		script := []byte("#!/bin/sh\n: > " + filepath.Join(marks, v) + "\nexec " + bin + "\n")
		sums := pluginregtest.SHA256SUMS(map[string][]byte{asset: script})
		if v == "v3" {
			sums = pluginregtest.SHA256SUMS(map[string][]byte{asset: []byte("something else")})
		}
		files["plugin/usage/"+v+"/"+asset], files["plugin/usage/"+v+"/SHA256SUMS"] = script, sums
	}
	srv := pluginregtest.ReleaseServer(t, files)
	old := pluginreg.ReleaseURL
	pluginreg.ReleaseURL = func(_, _, tag, asset string) string { return srv.URL + "/" + tag + "/" + asset }
	t.Cleanup(func() { pluginreg.ReleaseURL = old })

	cfg := noopModulesConfig(t, t.TempDir(), "noop: {}\nusage:\n  prometheus_url: http://prom:9090\n  exec: release\n")
	cfg.Plugins = &config.PluginsConfig{Root: t.TempDir()}
	reg := pluginreg.NewFSRegistry(cfg.Plugins.Root)
	pin := func(v string) {
		e, err := pluginreg.ParseEntry("github:o/r@plugin/usage/" + v + "#extensions/usage/plugin")
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Put(context.Background(), pluginreg.FromEntry(e)); err != nil {
			t.Fatal(err)
		}
	}
	ran := func(v string) bool { _, err := os.Stat(filepath.Join(marks, v)); return err == nil }
	build := func() ([]builtSDKExtension, error) {
		var orchRef atomic.Pointer[orchestrator.Orchestrator]
		var judgeModelRef atomic.Pointer[model.LLM]
		return buildSDKExtensions(cfg, nil, nil, nil, &orchRef, nil, nil, &judgeModelRef, nil, nil, nil, nil, nil, reg)
	}

	pin("v3")
	if _, err := build(); err == nil || !strings.Contains(err.Error(), "does not match") || ran("v3") {
		t.Fatalf("boot on a mismatched checksum: err %v, ran %t; want a checksum error and no child", err, ran("v3"))
	}

	pin("v1")
	exts, err := build()
	if err != nil {
		t.Fatalf("boot on v1: %v", err)
	}
	t.Cleanup(func() { closeSDKExtensions(exts) })
	if !ran("v1") {
		t.Fatal("boot did not run the v1 release binary")
	}

	h := &rest.Handler{}
	h.SetExtensionRestarter(extensionRestarter(cfg, reg, exts))
	restart := func(name string) int {
		w := httptest.NewRecorder()
		h.RestartExtension(w, httptest.NewRequest(http.MethodPost, "/", nil), name)
		return w.Code
	}
	pin("v2")
	if code := restart("usage"); code != http.StatusNoContent || !ran("v2") {
		t.Errorf("restart after pinning v2 = %d, v2 ran %t; want 204 and the v2 binary running", code, ran("v2"))
	}
	if code := restart("nope"); code != http.StatusNotFound {
		t.Errorf("restart of an unknown extension = %d, want 404", code)
	}
	if code := restart("noop"); code != http.StatusConflict {
		t.Errorf("restart of an in-process extension = %d, want 409", code)
	}

	pin("v3")
	if code := restart("usage"); code != http.StatusBadGateway || ran("v3") {
		t.Errorf("restart onto a mismatched checksum = %d, v3 ran %t; want 502 and no child", code, ran("v3"))
	}
	ts := httptest.NewServer(server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(exts)}))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/usage/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /usage/ after a failed restart = %d, want 200 from the v2 child", resp.StatusCode)
	}
}
