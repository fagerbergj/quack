// Package usage is an inbound-only Prometheus usage dashboard: a page and a narrow query
// proxy behind quack's session auth. No tools, no dispatch.
package usage

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "usage"

// defaultRangeWindow is used when config.DefaultRange is empty.
const defaultRangeWindow = 24 * time.Hour

func init() {
	sdk.Register(extensionName, factory)
}

// config is extensions.usage in quack.yaml; it must not redefine sdk.BaseConfig's keys.
type config struct {
	// PrometheusURL is the Prometheus HTTP API base, e.g. "http://prometheus:9090". Required.
	PrometheusURL string `yaml:"prometheus_url"`

	// TempoURL, if set, is handed to the page for a trace link; this extension never queries it.
	TempoURL string `yaml:"tempo_url"`

	// DefaultRange is a Go duration string, e.g. "24h". Defaults to 24h.
	DefaultRange string `yaml:"default_range"`
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("usage: parse config: %w", err)
	}

	if cfg.PrometheusURL == "" {
		return nil, fmt.Errorf("usage: prometheus_url is required")
	}
	promURL, err := absoluteURL(cfg.PrometheusURL)
	if err != nil {
		return nil, fmt.Errorf("usage: invalid prometheus_url: %w", err)
	}

	tempoURL := ""
	if cfg.TempoURL != "" {
		u, err := absoluteURL(cfg.TempoURL)
		if err != nil {
			return nil, fmt.Errorf("usage: invalid tempo_url: %w", err)
		}
		tempoURL = u.String()
	}

	rangeWindow := defaultRangeWindow
	if cfg.DefaultRange != "" {
		parsed, err := time.ParseDuration(cfg.DefaultRange)
		if err != nil {
			return nil, fmt.Errorf("usage: invalid default_range %q: %w", cfg.DefaultRange, err)
		}
		if parsed <= 0 {
			return nil, fmt.Errorf("usage: default_range must be positive, got %q", cfg.DefaultRange)
		}
		rangeWindow = parsed
	}

	return &extension{
		tempoURL:    tempoURL,
		rangeWindow: rangeWindow,
		proxy:       newPrometheusProxy(promURL.String(), host.Log),
	}, nil
}

// absoluteURL rejects anything without scheme+host, so a bad URL fails at config validation
// rather than deep inside the proxy on the first request.
func absoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("must be an absolute URL (scheme + host), got %q", raw)
	}
	return u, nil
}

type extension struct {
	tempoURL    string
	rangeWindow time.Duration
	proxy       *prometheusProxy
}

var (
	_ sdk.Extension = (*extension)(nil)
	_ sdk.UI        = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool { return nil }

// RegisterRoutes never uses public: the proxy only keeps Prometheus private while these
// routes stay behind session auth.
func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	authed.Get("/", e.handleDashboard)
	authed.Get("/app.js", e.handleAppJS)
	authed.Get("/api/query_range", e.proxy.handleQueryRange)
	authed.Get("/api/query", e.proxy.handleQuery)
}

func (e *extension) UI() sdk.UIDescriptor {
	return sdk.UIDescriptor{Title: "Usage", Href: "/usage/", Icon: "monitoring"}
}
