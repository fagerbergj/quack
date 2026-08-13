package serve

import (
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
)

// fakeUIExtension implements extsdk.Extension + extsdk.UI - a stand-in for
// a real SDK module (like the future reMarkable one) so the UI-descriptor
// type assertion in buildSDKExtensions can be tested without a real
// registered module implementing it (noop predates sdk.UI).
type fakeUIExtension struct{}

func (fakeUIExtension) Tools() []tool.Tool                       { return nil }
func (fakeUIExtension) RegisterRoutes(authed, public chi.Router) {}
func (fakeUIExtension) UI() extsdk.UIDescriptor {
	return extsdk.UIDescriptor{Title: "Fake UI", Href: "/fake-ui-test/home", Icon: "🧪"}
}

func init() {
	extsdk.Register("fake-ui-test", func(extsdk.Host, []byte) (extsdk.Extension, error) {
		return fakeUIExtension{}, nil
	})
}

// TestBuildSDKExtensions_UIDescriptorCaptured proves buildSDKExtensions
// type-asserts sdk.UI at build time and extensionDescriptors surfaces it -
// the wiring GET /api/v1/extensions depends on for a module WITH a UI
// descriptor.
func TestBuildSDKExtensions_UIDescriptorCaptured(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "fake-ui-test:\n  enabled: true\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 1 || sdkExts[0].name != "fake-ui-test" {
		t.Fatalf("sdkExts = %+v, want one fake-ui-test extension", sdkExts)
	}
	if sdkExts[0].title != "Fake UI" || sdkExts[0].href != "/fake-ui-test/home" || sdkExts[0].icon != "🧪" {
		t.Errorf("title/href/icon = %q/%q/%q, want the UI() descriptor's values", sdkExts[0].title, sdkExts[0].href, sdkExts[0].icon)
	}

	descs := extensionDescriptors(sdkExts)
	if len(descs) != 1 {
		t.Fatalf("extensionDescriptors = %+v, want 1", descs)
	}
	want := schema.ExtensionInfo{Name: "fake-ui-test", Title: strPtrForTest("Fake UI"), Href: strPtrForTest("/fake-ui-test/home"), Icon: strPtrForTest("🧪")}
	if descs[0].Name != want.Name || *descs[0].Title != *want.Title || *descs[0].Href != *want.Href || *descs[0].Icon != *want.Icon {
		t.Errorf("descs[0] = %+v, want %+v", descs[0], want)
	}
}

// TestBuildSDKExtensions_NoUIDescriptor_NameOnly covers noop, which
// implements no sdk.UI - the SPA nav must fall back to a name-only entry.
func TestBuildSDKExtensions_NoUIDescriptor_NameOnly(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]

	cfg := noopModulesConfig(t, t.TempDir(), "noop:\n  enabled: true\n")
	sdkExts, err := buildSDKExtensions(cfg, st, hub, &orchRef, artifacts, jail, &judgeModelRef, nil, nil)
	if err != nil {
		t.Fatalf("buildSDKExtensions: %v", err)
	}
	if len(sdkExts) != 1 || sdkExts[0].title != "" || sdkExts[0].href != "" || sdkExts[0].icon != "" {
		t.Fatalf("sdkExts = %+v, want noop's title/href/icon empty", sdkExts)
	}

	descs := extensionDescriptors(sdkExts)
	if len(descs) != 1 || descs[0].Name != "noop" || descs[0].Title != nil || descs[0].Href != nil || descs[0].Icon != nil {
		t.Errorf("descs = %+v, want name-only noop", descs)
	}
}

func strPtrForTest(s string) *string { return &s }
