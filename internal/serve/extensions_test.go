package serve

import (
	"context"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/vetting"
)

// fakeSDKRecoverer stands in for an extension's sdk.DeliveryRecoverer.
type fakeSDKRecoverer struct{ gotDC extsdk.DeliveryContext }

func (f *fakeSDKRecoverer) RecoverDelivery(_ context.Context, key string, dc extsdk.DeliveryContext) (bool, extsdk.DeliveryItemOutcome, error) {
	f.gotDC = dc
	return true, extsdk.DeliveryItemOutcome{Kind: "review", URL: "https://example/pr/1#review-1"}, nil
}

func TestSdkRecoverAdapterForwardsAndMapsOutcome(t *testing.T) {
	fake := &fakeSDKRecoverer{}
	adapter := sdkRecoverAdapter{recoverer: fake}

	found, outcome, err := adapter.RecoverDelivery(context.Background(), "key1", cli.DeliveryContext{
		CloneURL: "https://github.com/x/y.git", IssueNumber: 7,
	})
	if err != nil {
		t.Fatalf("RecoverDelivery: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if fake.gotDC.CloneURL != "https://github.com/x/y.git" || fake.gotDC.IssueNumber != 7 {
		t.Errorf("sdk saw DeliveryContext %+v, want CloneURL/IssueNumber forwarded", fake.gotDC)
	}
	if outcome.URL != "https://example/pr/1#review-1" || outcome.Kind != "review" {
		t.Errorf("outcome = %+v, want it mapped from the sdk outcome", outcome)
	}
}

// fakeDeliverer captures the sdk.DeliveryContext it receives so the test can
// assert sdkDeliverAdapter forwarded the fields quack's own DeliveryContext
// set (#1158 PushError, #1093 IdempotencyKey).
type fakeDeliverer struct{ got extsdk.DeliveryContext }

func (f *fakeDeliverer) Deliver(ctx context.Context, dc extsdk.DeliveryContext) ([]extsdk.DeliveryItemOutcome, error) {
	f.got = dc
	return nil, nil
}

func TestSdkDeliverAdapterForwardsPushErrorAndIdempotencyKey(t *testing.T) {
	fake := &fakeDeliverer{}
	adapter := sdkDeliverAdapter{deliverer: fake}

	_, err := adapter.Deliver(context.Background(), vetting.DeliveryContext{
		PushError:      "push rejected: non-fast-forward",
		IdempotencyKey: "artifact-42@3",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if fake.got.PushError != "push rejected: non-fast-forward" {
		t.Errorf("PushError = %q, want it forwarded from quack's DeliveryContext", fake.got.PushError)
	}
	if fake.got.IdempotencyKey != "artifact-42@3" {
		t.Errorf("IdempotencyKey = %q, want it forwarded from quack's DeliveryContext", fake.got.IdempotencyKey)
	}
}

func init() {
	// Registered once at package init (extsdk.Register panics on a repeat
	// name) under names no real extension uses, so BuildDeliveryRecoverer's
	// tests can drive them via ordinary config.
	extsdk.Register("fake-recoverer-a", func(host extsdk.Host, _ []byte) (extsdk.Extension, error) {
		return &recovererExt{}, nil
	})
	extsdk.Register("fake-recoverer-b", func(host extsdk.Host, _ []byte) (extsdk.Extension, error) {
		return &recovererExt{}, nil
	})
	extsdk.Register("fake-non-recoverer", func(host extsdk.Host, _ []byte) (extsdk.Extension, error) {
		return &nonRecovererExt{}, nil
	})
}

// recovererExt implements sdk.DeliveryRecoverer; nonRecovererExt doesn't -
// BuildDeliveryRecoverer must detect the difference via type assertion.
type recovererExt struct{}

func (recovererExt) Tools() []tool.Tool                    { return nil }
func (recovererExt) RegisterRoutes(chi.Router, chi.Router) {}
func (recovererExt) RecoverDelivery(context.Context, string, extsdk.DeliveryContext) (bool, extsdk.DeliveryItemOutcome, error) {
	return false, extsdk.DeliveryItemOutcome{}, nil
}

type nonRecovererExt struct{}

func (nonRecovererExt) Tools() []tool.Tool                    { return nil }
func (nonRecovererExt) RegisterRoutes(chi.Router, chi.Router) {}

func moduleNode(t *testing.T, yamlSrc string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(yamlSrc), &n); err != nil {
		t.Fatalf("unmarshal module node: %v", err)
	}
	// yaml.Unmarshal into a Node produces a DocumentNode wrapping the
	// mapping - ExtensionsConfig.Modules stores the mapping node itself.
	return *n.Content[0]
}

func testConfig(t *testing.T, modules map[string]string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Workspace.Root = t.TempDir()
	cfg.Extensions.Modules = map[string]yaml.Node{}
	for name, src := range modules {
		cfg.Extensions.Modules[name] = moduleNode(t, src)
	}
	return cfg
}

func TestBuildDeliveryRecoverer_NoImplementer(t *testing.T) {
	cfg := testConfig(t, map[string]string{"fake-non-recoverer": "enabled: true"})
	rec, name, err := BuildDeliveryRecoverer(cfg)
	if err != nil {
		t.Fatalf("BuildDeliveryRecoverer: %v", err)
	}
	if rec != nil || name != "" {
		t.Fatalf("got (%v, %q), want (nil, \"\") when no configured extension implements DeliveryRecoverer", rec, name)
	}
}

func TestBuildDeliveryRecoverer_DisabledModuleSkipped(t *testing.T) {
	cfg := testConfig(t, map[string]string{"fake-recoverer-a": "enabled: false"})
	rec, name, err := BuildDeliveryRecoverer(cfg)
	if err != nil {
		t.Fatalf("BuildDeliveryRecoverer: %v", err)
	}
	if rec != nil || name != "" {
		t.Fatalf("got (%v, %q), want (nil, \"\") for a disabled module", rec, name)
	}
}

func TestBuildDeliveryRecoverer_FirstInSortedOrderWins(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"fake-recoverer-a": "enabled: true",
		"fake-recoverer-b": "enabled: true",
	})
	_, name, err := BuildDeliveryRecoverer(cfg)
	if err != nil {
		t.Fatalf("BuildDeliveryRecoverer: %v", err)
	}
	if name != "fake-recoverer-a" {
		t.Fatalf("name = %q, want the first name in sorted order (fake-recoverer-a)", name)
	}
}
