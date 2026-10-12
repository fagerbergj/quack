package usage

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

func newTestHost() sdk.Host {
	return sdk.Host{}
}

// newChiRouterForTest mounts the authed routes on a bare chi router, as quack would.
func newChiRouterForTest(e *extension) http.Handler {
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())
	return r
}

func TestFactoryRequiresPrometheusURL(t *testing.T) {
	if _, err := factory(newTestHost(), []byte("default_range: 1h\n")); err == nil {
		t.Fatal("expected an error when prometheus_url is missing, got nil")
	}
}

func TestFactoryRejectsRelativePrometheusURL(t *testing.T) {
	if _, err := factory(newTestHost(), []byte("prometheus_url: /not-absolute\n")); err == nil {
		t.Fatal("expected an error for a non-absolute prometheus_url, got nil")
	}
}

func TestFactoryRejectsMalformedYAML(t *testing.T) {
	if _, err := factory(newTestHost(), []byte("prometheus_url: [not, a, string\n")); err == nil {
		t.Fatal("expected an error for malformed config, got nil")
	}
}

func TestFactoryRejectsInvalidTempoURL(t *testing.T) {
	raw := []byte("prometheus_url: http://prom:9090\ntempo_url: \"::not a url\"\n")
	if _, err := factory(newTestHost(), raw); err == nil {
		t.Fatal("expected an error for a malformed tempo_url, got nil")
	}
}

func TestFactoryRejectsInvalidDefaultRange(t *testing.T) {
	raw := []byte("prometheus_url: http://prom:9090\ndefault_range: not-a-duration\n")
	if _, err := factory(newTestHost(), raw); err == nil {
		t.Fatal("expected an error for an invalid default_range, got nil")
	}
}

func TestFactoryRejectsNonPositiveDefaultRange(t *testing.T) {
	raw := []byte("prometheus_url: http://prom:9090\ndefault_range: 0s\n")
	if _, err := factory(newTestHost(), raw); err == nil {
		t.Fatal("expected an error for a zero default_range, got nil")
	}
}

func TestFactoryDefaultsRangeTo24h(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	if e.rangeWindow != 24*time.Hour {
		t.Errorf("rangeWindow = %v, want 24h", e.rangeWindow)
	}
}

func TestFactoryHonorsExplicitDefaultRangeAndTempoURL(t *testing.T) {
	raw := []byte("prometheus_url: http://prom:9090\ntempo_url: http://tempo:3200\ndefault_range: 1h\n")
	extVal, err := factory(newTestHost(), raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	if e.rangeWindow != time.Hour {
		t.Errorf("rangeWindow = %v, want 1h", e.rangeWindow)
	}
	if e.tempoURL != "http://tempo:3200" {
		t.Errorf("tempoURL = %q, want http://tempo:3200", e.tempoURL)
	}
}

func TestFactorySideEffectFree(t *testing.T) {
	// A bogus, unreachable prometheus_url must still construct cleanly -
	// factories validate config, they never dial out (see sdk.Factory's doc).
	raw := []byte("prometheus_url: http://usage-test-host-does-not-resolve.invalid:9090\n")
	if _, err := factory(newTestHost(), raw); err != nil {
		t.Fatalf("factory should not attempt network I/O, got: %v", err)
	}
}

func TestUIDescriptor(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	ui := e.UI()
	if ui.Title != "Usage" {
		t.Errorf("Title = %q, want Usage", ui.Title)
	}
	if ui.Href != "/usage/" {
		t.Errorf("Href = %q, want /usage/", ui.Href)
	}
	if ui.Icon != "monitoring" {
		t.Errorf("Icon = %q, want monitoring", ui.Icon)
	}
}

func TestToolsIsEmpty(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	if len(e.Tools()) != 0 {
		t.Errorf("Tools() = %v, want empty - usage is inbound-only", e.Tools())
	}
}

func TestNothingRegisteredOnPublicRouter(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)

	authed := chi.NewRouter()
	public := chi.NewRouter()
	e.RegisterRoutes(authed, public)

	if len(public.Routes()) != 0 {
		t.Errorf("public router got %d routes, want 0 - usage must be authed-only", len(public.Routes()))
	}
	if len(authed.Routes()) == 0 {
		t.Error("authed router got 0 routes, want the dashboard + proxy routes")
	}
}

func TestRegisteredInInit(t *testing.T) {
	factories := sdk.Registered()
	if _, ok := factories["usage"]; !ok {
		t.Fatal(`sdk.Registered() missing "usage" - init() should have registered it`)
	}
}

// An empty extensions.usage block must reach validation, not fail as a YAML parse error.
func TestFactoryEmptyConfig(t *testing.T) {
	_, err := factory(newTestHost(), nil)
	if err == nil || !strings.Contains(err.Error(), "prometheus_url is required") {
		t.Fatalf("factory(nil raw) = %v, want prometheus_url is required", err)
	}
}
