// Package sleeper is the Sleeper fantasy-football extension: read-only agent tools, a UI page, daily
// snapshots, and observe-only decision points.
package sleeper

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "sleeper"

func init() {
	sdk.Register(extensionName, factory)
}

// config is the YAML under extensions.sleeper; Enabled/DataDir mirror sdk.BaseConfig's reserved keys so
// KnownFields decoding doesn't reject them.
type config struct {
	Enabled   *bool  `yaml:"enabled"`
	DataDir   string `yaml:"data_dir"`
	Snapshots string `yaml:"snapshots"`

	// DefaultUser is a Sleeper username or user_id; tools fall back to it when a chat names none.
	DefaultUser string `yaml:"default_user"`

	// DefaultLeague lets the orchestrator omit league_id in chat.
	DefaultLeague string `yaml:"default_league"`

	// Timezone overrides Host.Location for *_local fields; an IANA name like America/Chicago.
	Timezone string `yaml:"timezone"`
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	cfg := config{}
	if len(raw) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("sleeper: parse config: %w", err)
		}
	}
	switch cfg.Snapshots {
	case "", "off", "daily":
	default:
		return nil, fmt.Errorf("sleeper: snapshots must be \"off\" or \"daily\", got %q", cfg.Snapshots)
	}
	loc := hostZone(host)
	if cfg.Timezone != "" {
		l, err := loadZone(cfg.Timezone)
		if err != nil {
			return nil, fmt.Errorf("sleeper: timezone: %w", err)
		}
		loc = l
	}

	client, err := NewClient(sleeperBaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sleeper: new client: %w", err)
	}
	return &extension{host: host, cfg: cfg, client: client, loc: loc}, nil
}

// hostZone is quack's zone when region-style (quack accepts EST, a fixed offset an hour off in DST);
// nil means time.Local.
func hostZone(host sdk.Host) *time.Location {
	if host.Location == nil {
		return nil
	}
	if _, err := loadZone(host.Location.String()); err != nil {
		if host.Log != nil {
			host.Log.Warn("sleeper: ignoring quack's timezone; using the process zone", "err", err)
		}
		return nil
	}
	return host.Location
}

// sleeperBaseURL is the production API; tests point a Client at cmd/qa-mock instead.
const sleeperBaseURL = "https://api.sleeper.app"

type extension struct {
	host   sdk.Host
	cfg    config
	client *Client
	loc    *time.Location // config timezone, else Host.Location; nil = time.Local

	// running holds global chat ids dispatched here that haven't RunEnded; in-memory, so a restart only
	// loses Running badges.
	runningMu sync.Mutex
	running   map[string]struct{}
}

var (
	_ sdk.Extension       = (*extension)(nil)
	_ sdk.UI              = (*extension)(nil)
	_ sdk.RunObserver     = (*extension)(nil)
	_ sdk.ArtifactSchemas = (*extension)(nil)
	_ sdk.DecisionPoints  = (*extension)(nil)
)

// Tools returns the read-only agent tools over the Sleeper client.
func (e *extension) Tools() []tool.Tool {
	return []tool.Tool{
		e.userTool(),
		e.leagueTool(),
		e.rosterTool(),
		e.matchupTool(),
		e.standingsTool(),
		e.transactionsTool(),
		e.freeAgentsTool(),
		e.playerTool(),
		e.scheduleTool(),
		e.trendsTool(),
		e.draftTool(),
		e.historyTool(),
	}
}

// RegisterRoutes mounts the UI page and its JSON API.
func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) { e.mountUI(authed) }

// footballIcon is Material Symbols "sports_football" inlined: quack's nav rail renders only its own icon
// names or an inline <svg>.
const footballIcon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true"><path d="M480-480ZM362-202 202-362q-3 38-1.5 79t7.5 73q23 7 69.5 9t84.5-1Zm96-16q59-13 106-37t82-59q34-34 58-80.5T742-500L500-742q-57 14-103 38.5T316-644q-35 35-59.5 81.5T218-458l240 240Zm-62-122-56-56 224-224 56 56-224 224Zm362-256q4-39 2.5-81t-8.5-73q-23-8-69.5-10t-84.5 2l160 162ZM310-120q-57 0-104-8.5T148-148q-11-12-19.5-60T120-314q0-119 36-220.5T258-702q66-66 169-102t223-36q58 0 104.5 8.5T812-812q11 12 19.5 60t8.5 108q0 117-36 218.5T702-258q-65 65-168 101.5T310-120Z"/></svg>`

// UI names the nav entry for the page that renders job artifacts and launches runs.
func (e *extension) UI() sdk.UIDescriptor {
	return sdk.UIDescriptor{Title: "Sleeper", Href: "/sleeper/", Icon: footballIcon}
}

var _ sdk.Starter = (*extension)(nil)

// Start runs the daily snapshot ticker when configured with a default_league; it never dispatches.
func (e *extension) Start(ctx context.Context) error {
	if e.cfg.Snapshots != "daily" || e.cfg.DefaultLeague == "" {
		return nil
	}
	go e.runSnapshotTicker(ctx)
	return nil
}
