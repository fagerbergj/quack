package serve

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"

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

// SeedPluginAgentsAndShapes merges admitted plugins' agents and workflow shapes into cfg (gated by
// pluginGateEnabled) and returns the overrides no plugin claimed. Call before buildAgents.
func SeedPluginAgentsAndShapes(cfg *config.Config, plugins []plugin.Plugin) ([]PluginSeedResult, []string, error) {
	var results []PluginSeedResult
	for _, p := range plugins {
		r, err := seedPlugin(cfg, p)
		if err != nil {
			return nil, nil, err
		}
		if len(r.Agents) > 0 || len(r.Shapes) > 0 {
			results = append(results, r)
		}
	}
	return results, dropUnclaimedOverrides(cfg), nil
}

// seedPlugin is one plugin's share of SeedPluginAgentsAndShapes, without the
// final unclaimed-override drop (a later plugin may still claim one).
func seedPlugin(cfg *config.Config, p plugin.Plugin) (PluginSeedResult, error) {
	r := PluginSeedResult{Plugin: p.Name}
	if p.AgentsDir == "" && p.WorkflowsDir == "" {
		return r, nil
	}
	enabled, err := pluginGateEnabled(cfg, p)
	if err != nil || !enabled {
		return r, err
	}
	if p.AgentsDir != "" {
		if r.Agents, err = cfg.SeedPluginAgents(p.Name, p.AgentsDir, p.Agents); err != nil {
			return r, err
		}
	}
	if p.WorkflowsDir != "" {
		if r.Shapes, err = cfg.SeedPluginShapes(p.Name, p.WorkflowsDir, p.Workflows); err != nil {
			return r, err
		}
	}
	return r, nil
}

// dropUnclaimedOverrides removes every bundle:-less entry no plugin seeded (only legal as a plugin
// override, so a missing plugin costs the agent, not the boot); it returns the non-optional ones.
func dropUnclaimedOverrides(cfg *config.Config) []string {
	var dropped []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Agents)) {
		ac := cfg.Agents[name]
		if ac.Bundle != "" {
			continue
		}
		delete(cfg.Agents, name)
		if ac.Optional {
			slog.Warn("optional agent has no plugin-supplied bundle; dropped from the roster", "component", "startup", "agent", name)
			continue
		}
		dropped = append(dropped, name)
		slog.Error(fmt.Sprintf("no plugin seeded agent %q (plugin unfetched/refused or module disabled); override dropped", name), "component", "startup")
	}
	return dropped
}

// pluginGateEnabled: a plugin naming no linked module seeds unconditionally;
// one naming modules seeds only when every declared module is enabled.
func pluginGateEnabled(cfg *config.Config, p plugin.Plugin) (bool, error) {
	if len(p.Modules) == 0 {
		return true, nil
	}
	return pluginModuleGateEnabled(cfg.Extensions.Modules, p)
}

// pluginModuleGateEnabled is pluginGateEnabled's modules-map form, for
// admitPlugins/ResolveConfiguredPlugins, which carry the map but not a full *config.Config.
func pluginModuleGateEnabled(modules map[string]yaml.Node, p plugin.Plugin) (bool, error) {
	for _, m := range p.Modules {
		enabled, err := moduleEnabledIn(modules, m.Name)
		if err != nil {
			return false, err
		}
		if !enabled {
			return false, nil
		}
	}
	return true, nil
}

// moduleEnabledIn reports whether extensions.<name> is configured and not explicitly disabled.
func moduleEnabledIn(modules map[string]yaml.Node, name string) (bool, error) {
	if _, ok := modules[name]; !ok {
		return false, nil
	}
	_, base, err := parseModuleConfig(modules, name)
	return err == nil && (base.Enabled == nil || *base.Enabled), err
}

// parseModuleConfig re-marshals extensions.<name> and parses its shared BaseConfig fields.
func parseModuleConfig(modules map[string]yaml.Node, name string) ([]byte, extsdk.BaseConfig, error) {
	var base extsdk.BaseConfig
	node := modules[name]
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return nil, base, fmt.Errorf("extensions.%s: re-marshal config: %w", name, err)
	}
	if err := yaml.Unmarshal(raw, &base); err != nil {
		return nil, base, fmt.Errorf("extensions.%s: parse base config: %w", name, err)
	}
	return raw, base, nil
}

// ResolveConfiguredPlugins resolves plugins.seed from disk only: no registry write, git fetch or store
// open. A github: entry with no local clone is reported unresolvable.
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
	claims := newManifestClaims()
	for _, p := range plugins {
		plugin.WarnUnlistedManifestEntries(p)
		// Every row here came from plugins.seed, so a refusal is fatal -
		// the same treatment admitPlugins gives a seed row at boot.
		if err := admitOnePlugin(p, cfg.Extensions.Modules, claims); err != nil {
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
