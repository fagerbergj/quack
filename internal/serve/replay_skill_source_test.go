package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/replay"
)

// writeAgentInvokeReplayFixture writes one literal agent.invoke ledger line
// recording pluginsJSON - the exact wire shape #1427 P1 emits (quack.plugins
// -> AgentInvokePayload.Plugins), not built via ledger structs.
func writeAgentInvokeReplayFixture(t *testing.T, pluginsJSON string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"seq":1,"chat_id":"c","node_id":"n","agent":"impl","round":"worker-r0","kind":"agent.invoke","at":"2026-01-01T00:00:00Z","payload":{` +
		`"sent":"[]","received":"[]","plugins":` + pluginsJSON +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// registryWithFetchedPlugin fetches a fixture repo (plugin.json + one skill)
// into a fresh registry root and returns the root and the installed sha.
func registryWithFetchedPlugin(t *testing.T, name, body string) (root, sha string) {
	t.Helper()
	bare, work := pluginregtest.NewFixtureRepo(t)
	pluginregtest.RunGit(t, work, "rm", "--quiet", "SKILL.md")
	if err := os.WriteFile(filepath.Join(work, "plugin.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(work, "skills", "dothing")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pluginregtest.RunGit(t, work, "add", ".")
	pluginregtest.RunGit(t, work, "commit", "--quiet", "-m", "v1")
	pluginregtest.RunGit(t, work, "push", "--quiet", "origin", "main")

	prev := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string { return bare }
	t.Cleanup(func() { pluginreg.RemoteURL = prev })

	root = t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	e, err := pluginreg.ParseEntry("github:acme/" + name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reg.Fetch(context.Background(), pluginreg.FromEntry(e))
	if err != nil {
		t.Fatal(err)
	}
	return root, got.SHA
}

// TestReplaySkillSource_NonReplayConfig mirrors replayPromptSource's own
// "not a replay" case: no replay provider, no wiring, live source unchanged.
func TestReplaySkillSource_NonReplayConfig(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"p": {Kind: "openai"}}}
	src, err := replaySkillSource(context.Background(), cfg, nil, nil)
	if err != nil || src != nil {
		t.Fatalf("replaySkillSource(non-replay) = %v, %v; want nil, nil", src, err)
	}
}

// TestReplaySkillSource_NoPluginsRecorded is the P1-native-only bundle case:
// a replay config whose bundle recorded no agent.invoke plugin provenance
// (no ACP round in it) leaves the live source in place.
func TestReplaySkillSource_NoPluginsRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"seq":1,"chat_id":"c","kind":"llm.call","at":"2026-01-01T00:00:00Z","payload":{"request_model":"any-model"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"replay-test": {Kind: "replay", Bundle: path}},
		Plugins:   &config.PluginsConfig{Root: t.TempDir()},
	}
	src, err := replaySkillSource(context.Background(), cfg, nil, nil)
	if err != nil || src != nil {
		t.Fatalf("replaySkillSource(no recorded plugins) = %v, %v; want nil, nil", src, err)
	}
}

// TestReplaySkillSource_ServesRecordedSHA is #1432's core case: a bundle
// recording the plugin at the OLD sha replays the OLD skill text even after
// the registry has moved on to a new one.
func TestReplaySkillSource_ServesRecordedSHA(t *testing.T) {
	root, sha1 := registryWithFetchedPlugin(t, "widgets", "---\nname: dothing\ndescription: v1\n---\nbody v1")
	bundle := writeAgentInvokeReplayFixture(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"replay-test": {Kind: "replay", Bundle: bundle}},
		Plugins:   &config.PluginsConfig{Root: root},
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src, err := replaySkillSource(context.Background(), cfg, rows, nil)
	if err != nil {
		t.Fatalf("replaySkillSource: %v", err)
	}
	if src == nil {
		t.Fatal("replaySkillSource(recorded plugin) = nil, want a source")
	}
	fm, err := src.LoadFrontmatter(context.Background(), "widgets:dothing")
	if err != nil {
		t.Fatalf("LoadFrontmatter: %v", err)
	}
	if fm.Description != "v1" {
		t.Fatalf("description = %q, want v1", fm.Description)
	}
}

// TestReplaySkillSource_DeletedCloneRefuses is #1432's LOAD-time refusal:
// initSkills must fail boot, not a later round, when the clone is gone.
func TestReplaySkillSource_DeletedCloneRefuses(t *testing.T) {
	root, sha1 := registryWithFetchedPlugin(t, "widgets", "---\nname: dothing\ndescription: v1\n---\nbody v1")
	if err := os.RemoveAll(pluginreg.CloneDir(root, "widgets")); err != nil {
		t.Fatal(err)
	}
	bundle := writeAgentInvokeReplayFixture(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"replay-test": {Kind: "replay", Bundle: bundle}},
		Plugins:   &config.PluginsConfig{Root: root},
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = replaySkillSource(context.Background(), cfg, rows, nil)
	if err == nil || !strings.Contains(err.Error(), "widgets") {
		t.Fatalf("err = %v, want a refusal naming widgets", err)
	}
}

// TestRefuseIfPluginsMoved is the ACP fork-mode guard: a live spawn after
// divergence reads the CURRENT registry clone, so a plugin that moved past
// its recorded sha must refuse rather than silently serve different text.
func TestRefuseIfPluginsMoved(t *testing.T) {
	root, sha1 := registryWithFetchedPlugin(t, "widgets", "---\nname: dothing\ndescription: v1\n---\nbody v1")

	t.Run("unmoved plugin is fine", func(t *testing.T) {
		bundle := writeAgentInvokeReplayFixture(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
		sess, err := replay.Load(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseIfPluginsMoved(sess, pluginreg.NewFSRegistry(root)); err != nil {
			t.Fatalf("refuseIfPluginsMoved: %v", err)
		}
	})

	t.Run("moved plugin refuses naming it", func(t *testing.T) {
		bundle := writeAgentInvokeReplayFixture(t, `[{"name":"widgets","sha":"`+strings.Repeat("a", 40)+`"}]`)
		sess, err := replay.Load(bundle)
		if err != nil {
			t.Fatal(err)
		}
		err = refuseIfPluginsMoved(sess, pluginreg.NewFSRegistry(root))
		if err == nil || !strings.Contains(err.Error(), "widgets") {
			t.Fatalf("err = %v, want a refusal naming widgets", err)
		}
	})

	t.Run("local/embedded (no sha) never refuses", func(t *testing.T) {
		bundle := writeAgentInvokeReplayFixture(t, `[{"name":"quack","sha":""}]`)
		sess, err := replay.Load(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := refuseIfPluginsMoved(sess, pluginreg.NewFSRegistry(root)); err != nil {
			t.Fatalf("refuseIfPluginsMoved: %v", err)
		}
	})

	// F5: a recorded plugin no longer in the registry at all names a clear
	// "no longer registered" reason, not the empty installed-sha message.
	t.Run("removed plugin names itself, not a blank sha", func(t *testing.T) {
		bundle := writeAgentInvokeReplayFixture(t, `[{"name":"gone","sha":"`+sha1+`"}]`)
		sess, err := replay.Load(bundle)
		if err != nil {
			t.Fatal(err)
		}
		err = refuseIfPluginsMoved(sess, pluginreg.NewFSRegistry(root))
		if err == nil || !strings.Contains(err.Error(), "no longer registered") {
			t.Fatalf("err = %v, want containing %q", err, "no longer registered")
		}
	})
}
