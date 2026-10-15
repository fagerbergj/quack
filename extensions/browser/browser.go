// Package browser lets agents drive a real Chromium over CDP: open allowed pages, observe numbered
// elements and a screenshot, act on an element, and ask the decision model which element to pick.
package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "browser"

func init() { sdk.Register(extensionName, factory) }

// config is the YAML under extensions.browser; Enabled/DataDir/Exec mirror sdk.BaseConfig's reserved keys so
// KnownFields decoding doesn't reject them.
type config struct {
	Enabled *bool  `yaml:"enabled"`
	DataDir string `yaml:"data_dir"`
	Exec    string `yaml:"exec"`

	// CDPURL is the browser-level DevTools websocket, e.g. a browserless container's ws://host:3000.
	CDPURL string `yaml:"cdp_url"`

	// AllowedHosts are the exact hostnames a page may load; empty refuses every navigation.
	AllowedHosts []string `yaml:"allowed_hosts"`

	// MaxElements caps an observation's element list.
	MaxElements int `yaml:"max_elements"`
}

const defaultMaxElements = 50

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	cfg := config{}
	if len(raw) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("browser: parse config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	e := &extension{host: host, cfg: cfg, pages: map[string]*page{}}
	e.browser = &cdpBrowser{url: cfg.CDPURL, allowed: e.allowed}
	return e, nil
}

func (c *config) validate() error {
	u, err := url.Parse(c.CDPURL)
	if c.CDPURL == "" || err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("browser: cdp_url must be a ws:// or wss:// DevTools URL, got %q", c.CDPURL)
	}
	if c.MaxElements < 0 {
		return fmt.Errorf("browser: max_elements must be positive, got %d", c.MaxElements)
	}
	if c.MaxElements == 0 {
		c.MaxElements = defaultMaxElements
	}
	for i, h := range c.AllowedHosts {
		if h == "" || strings.ContainsAny(h, ":/*") {
			return fmt.Errorf("browser: allowed_hosts[%d] must be a bare hostname, got %q", i, h)
		}
		c.AllowedHosts[i] = strings.ToLower(h)
	}
	return nil
}

// Browser opens tabs; Tab is one of them. The CDP client implements both; tests use a fake.
type Browser interface {
	NewTab(ctx context.Context) (Tab, error)
}

type Tab interface {
	Navigate(ctx context.Context, url string) error
	URL(ctx context.Context) (string, error)

	// Snapshot tags the page's interactive elements in document order; Act's index is a position in it.
	Snapshot(ctx context.Context) (Snapshot, error)
	Act(ctx context.Context, index int, action, text string) error
	Close(ctx context.Context) error
}

type Snapshot struct {
	Title, URL string
	Elements   []Element
	JPEG       []byte
}

type Element struct {
	Role string `json:"role"`
	Name string `json:"name"`
}

type extension struct {
	host    sdk.Host
	cfg     config
	browser Browser

	mu    sync.Mutex
	next  int
	pages map[string]*page // ponytail: tabs live until the CDP connection drops; add browser_close if they pile up
}

// page is one open tab and its latest observation, the only one browser_act and browser_choose accept.
type page struct {
	mu    sync.Mutex // one action at a time per tab
	chat  string
	tab   Tab
	last  observation
	image string // latest screenshot as a data URL, for browser_choose
}

var (
	_ sdk.Extension      = (*extension)(nil)
	_ sdk.DecisionPoints = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool {
	return []tool.Tool{e.openTool(), e.observeTool(), e.actTool(), e.chooseTool()}
}

func (e *extension) RegisterRoutes(chi.Router, chi.Router) {}

// chooseQuestion's options are each call's numbered elements, so it declares no Criteria.
const chooseQuestion = "element"

func (e *extension) DecisionPoints() []sdk.DecisionPoint {
	return []sdk.DecisionPoint{{
		Name:        "element.choose",
		Description: "which element on a page the agent should act on next to reach its goal",
		Questions: map[string]sdk.DecisionQuestion{chooseQuestion: {Type: "choice",
			Instructions: "Which element should be clicked or filled next to accomplish the goal in the state?"}},
		Primary: chooseQuestion,
	}}
}

// allowed reports whether raw may load: about:blank, or http(s) on an allowed host.
func (e *extension) allowed(raw string) bool {
	if raw == "about:blank" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, a := range e.cfg.AllowedHosts {
		if h == a {
			return true
		}
	}
	return false
}
