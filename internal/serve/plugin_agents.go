package serve

import (
	"fmt"
	"log/slog"
	"os"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
)

// PluginSeedResult is one plugin's contribution to the roster, for boot's
// info log line and `server validate`'s equivalent report.
type PluginSeedResult struct {
	Plugin string   `json:"plugin"`
	Agents []string `json:"agents,omitempty"`
	Shapes []string `json:"shapes,omitempty"`
}

// SeedPluginAgentsAndShapes merges every admitted plugin's agents/ bundles
// and workflows/ shapes into cfg, gated per plugin on pluginGateEnabled.
// Call before workflowcatalog.FromConfig/buildAgents so both see the merge.
func SeedPluginAgentsAndShapes(cfg *config.Config, plugins []plugin.Plugin) ([]PluginSeedResult, error) {
	var results []PluginSeedResult
	for _, p := range plugins {
		if p.AgentsDir == "" && p.WorkflowsDir == "" {
			continue
		}
		enabled, err := pluginGateEnabled(cfg, p)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		var agents, shapes []string
		if p.AgentsDir != "" {
			if agents, err = cfg.SeedPluginAgents(p.Name, p.AgentsDir); err != nil {
				return nil, err
			}
		}
		if p.WorkflowsDir != "" {
			if shapes, err = cfg.SeedPluginShapes(p.Name, p.WorkflowsDir); err != nil {
				return nil, err
			}
		}
		if len(agents) > 0 || len(shapes) > 0 {
			results = append(results, PluginSeedResult{Plugin: p.Name, Agents: agents, Shapes: shapes})
		}
	}
	return results, nil
}

// pluginGateEnabled: a plugin naming no linked module seeds unconditionally;
// one naming modules seeds only when every declared module is enabled.
func pluginGateEnabled(cfg *config.Config, p plugin.Plugin) (bool, error) {
	if len(p.Modules) == 0 {
		return true, nil
	}
	for _, m := range p.Modules {
		enabled, err := moduleEnabled(cfg, m.Name)
		if err != nil {
			return false, err
		}
		if !enabled {
			return false, nil
		}
	}
	return true, nil
}

// moduleEnabled reports whether extensions.<name> is configured and not
// explicitly disabled - the same decision buildOneSDKExtension makes before
// mounting the module itself; both call this one helper.
func moduleEnabled(cfg *config.Config, name string) (bool, error) {
	node, ok := cfg.Extensions.Modules[name]
	if !ok {
		return false, nil
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return false, fmt.Errorf("extensions.%s: re-marshal config: %w", name, err)
	}
	var base extsdk.BaseConfig
	if err := yaml.Unmarshal(raw, &base); err != nil {
		return false, fmt.Errorf("extensions.%s: parse base config: %w", name, err)
	}
	return base.Enabled == nil || *base.Enabled, nil
}

// ResolveConfiguredPlugins resolves every plugins.seed row from what is
// already on disk - no registry seed/write, no git fetch, no store open.
// A github: entry with no local clone yet is reported unresolvable, never fetched.
func ResolveConfiguredPlugins(cfg *config.Config) (resolved []plugin.Plugin, unresolvable []string, err error) {
	var rows []pluginreg.Plugin
	for _, s := range cfg.Plugins.Seed {
		entry, err := pluginreg.ParseEntry(s)
		if err != nil {
			return nil, nil, err
		}
		row := pluginreg.FromEntry(entry)
		if row.Source == pluginreg.SourceGitHub {
			if st, statErr := os.Stat(row.Root(cfg.Plugins.Root)); statErr != nil || !st.IsDir() {
				unresolvable = append(unresolvable, row.Name)
				continue
			}
		}
		rows = append(rows, row)
	}
	plugins, err := resolveRegistryPlugins(cfg.Plugins.Root, rows)
	if err != nil {
		return nil, unresolvable, err
	}
	for _, p := range plugins {
		// Every row here came from plugins.seed, so a refusal is fatal -
		// the same treatment admitPlugins gives a seed row at boot.
		if err := checkPlugin(p, cfg.Extensions.Modules); err != nil {
			return nil, unresolvable, err
		}
	}
	return plugins, unresolvable, nil
}

// logPluginSeeds emits boot's one info line per plugin that actually seeded
// something - a disabled or agent/shape-less plugin logs nothing here.
func logPluginSeeds(results []PluginSeedResult) {
	for _, r := range results {
		slog.Info("plugin seeded agents and shapes", "component", "startup", "plugin", r.Plugin, "agents", r.Agents, "shapes", r.Shapes)
	}
}
