// Package android lets agents drive an Android emulator over the adb host protocol: observe numbered
// elements and a screenshot, tap or type on an element, launch allowed apps, and ask the decision model
// which element to pick.
package android

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "android"

func init() { sdk.Register(extensionName, factory) }

// config is the YAML under extensions.android; Enabled/DataDir/Exec mirror sdk.BaseConfig's reserved keys so
// KnownFields decoding doesn't reject them.
type config struct {
	Enabled *bool  `yaml:"enabled"`
	DataDir string `yaml:"data_dir"`
	Exec    string `yaml:"exec"`

	// ADBAddr is the adb server's host:port, e.g. an emulator container's 5037.
	ADBAddr string `yaml:"adb_addr"`

	// Serial picks a device; empty uses the only one the adb server has.
	Serial string `yaml:"serial"`

	// AllowedPackages are the apps an agent may act inside or launch; empty refuses every action.
	AllowedPackages []string `yaml:"allowed_packages"`

	// MaxElements caps an observation's element list.
	MaxElements int `yaml:"max_elements"`
}

const (
	defaultMaxElements = 50
	// settle is how long an action waits before re-reading the screen; adb has no "UI idle" signal.
	settle = 700 * time.Millisecond
)

var packageName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	cfg := config{}
	if len(raw) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("android: parse config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &extension{host: host, cfg: cfg, adb: &adbClient{addr: cfg.ADBAddr, serial: cfg.Serial}, settle: settle}, nil
}

func (c *config) validate() error {
	if _, port, err := net.SplitHostPort(c.ADBAddr); err != nil || port == "" {
		return fmt.Errorf("android: adb_addr must be host:port, got %q", c.ADBAddr)
	}
	if c.MaxElements < 0 {
		return fmt.Errorf("android: max_elements must be positive, got %d", c.MaxElements)
	}
	if c.MaxElements == 0 {
		c.MaxElements = defaultMaxElements
	}
	for i, p := range c.AllowedPackages {
		if !packageName.MatchString(p) {
			return fmt.Errorf("android: allowed_packages[%d] must be an Android package name, got %q", i, p)
		}
	}
	return nil
}

type extension struct {
	host   sdk.Host
	cfg    config
	adb    *adbClient
	settle time.Duration

	mu   sync.Mutex // one device, so one tool call at a time
	last screen     // latest observation, the only one android_act and android_choose accept
}

var (
	_ sdk.Extension      = (*extension)(nil)
	_ sdk.DecisionPoints = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool {
	return []tool.Tool{e.observeTool(), e.actTool(), e.launchTool(), e.chooseTool()}
}

func (e *extension) RegisterRoutes(chi.Router, chi.Router) {}

// chooseQuestion's options are each call's numbered elements, so it declares no Criteria.
const chooseQuestion = "element"

func (e *extension) DecisionPoints() []sdk.DecisionPoint {
	return []sdk.DecisionPoint{{
		Name:        "element.choose",
		Description: "which element on an Android screen the agent should act on next to reach its goal",
		Questions: map[string]sdk.DecisionQuestion{chooseQuestion: {Type: "choice",
			Instructions: "Which element should be tapped or typed into next to accomplish the goal in the state?"}},
		Primary: chooseQuestion,
	}}
}

func (e *extension) allowed(pkg string) bool { return slices.Contains(e.cfg.AllowedPackages, pkg) }
