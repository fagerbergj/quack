package plugin

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// manifest lays down a root carrying only plugin.json.
func manifest(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), body)
	return root
}

// A plugin with no skills/ is legal (spec §6.2: an absent fixed component
// location MUST NOT be an error). It used to be dropped entirely, which made
// a module-only or MCP-only plugin unloadable.
func TestResolve_NoSkillsDirIsNotAnError(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"toolsonly"}`)
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d plugins, want 1", len(got))
	}
	if got[0].SkillsDir != "" {
		t.Errorf("SkillsDir = %q, want empty", got[0].SkillsDir)
	}
	if got[0].Name != "toolsonly" {
		t.Errorf("Name = %q, want toolsonly", got[0].Name)
	}
}

// §8: a client MUST ignore namespaces it does not implement WITHOUT
// validating their contents. Garbage under someone else's key is not our
// problem and must never fail a load.
func TestResolve_ForeignNamespacesIgnoredUnvalidated(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{
		"com.example.client":{"anything":[1,2,3],"nested":{"totally":"unvalidated"}},
		"dev.other.thing":"not even an object"
	}}`)
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || len(got[0].Modules) != 0 || got[0].ConfigRequired {
		t.Fatalf("got %+v, want one plugin with no quack declarations", got)
	}
}

func TestResolve_OurNamespaceParsed(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"usage","extensions":{"`+Namespace+`":{
		"schemaVersion":1,
		"modules":[{"name":"usage","path":"github.com/fagerbergj/quack-extensions/usage"}],
		"config":"required"
	}}}`)
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	p := got[0]
	if !p.ConfigRequired {
		t.Error("ConfigRequired = false, want true")
	}
	if len(p.Modules) != 1 || p.Modules[0].Name != "usage" || p.Modules[0].Path != "github.com/fagerbergj/quack-extensions/usage" {
		t.Errorf("Modules = %+v", p.Modules)
	}
}

// Inside our own namespace §8 makes validation ours to define, and a block
// declaring compiled-in code is the one plugin failure quack refuses to
// downgrade to a warning.
func TestResolve_OurNamespaceInvalidIsAnError(t *testing.T) {
	cases := map[string]string{
		"wrong schemaVersion": `{"schemaVersion":99}`,
		"unknown field":       `{"schemaVersion":1,"whatIsThis":true}`,
		"module missing path": `{"schemaVersion":1,"modules":[{"name":"usage"}]}`,
		"bad config value":    `{"schemaVersion":1,"config":"sometimes"}`,
		"missing version":     `{"modules":[]}`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":`+block+`}}`)
			if _, err := Resolve([]string{root}); err == nil {
				t.Fatal("Resolve = nil error, want a namespace error")
			}
		})
	}
}

// ResolveSkillDirs stays warn-and-skip even when a namespace block is broken:
// the skills path never fails the run, boot surfaces the error separately.
func TestResolveSkillDirs_SurvivesNamespaceError(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{"schemaVersion":99}}}`)
	if dirs := resolveSkillDirs([]string{root}); len(dirs) != 0 {
		t.Fatalf("dirs = %v, want none", dirs)
	}
}

// A namespace block's agents/workflows lists decode and, when every listed
// name is present on disk, are carried onto Plugin unchanged.
func TestResolve_ManifestListsDecodeAndMatchPresentEntries(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"agents":["scout"],"workflows":["job"]
	}}}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)
	writeFile(t, filepath.Join(root, "workflows", "job.yaml"), "name: job")

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	p := got[0]
	if len(p.Agents) != 1 || p.Agents[0] != "scout" {
		t.Errorf("Agents = %v, want [scout]", p.Agents)
	}
	if len(p.Workflows) != 1 || p.Workflows[0] != "job" {
		t.Errorf("Workflows = %v, want [job]", p.Workflows)
	}
}

// A listed agent whose agents/<name>/ directory is absent is a
// NamespaceError-class failure naming the plugin and the entry - boot refuses
// the plugin rather than silently seeding nothing.
func TestResolve_ManifestListedAgentMissingFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"agents":["ghost"]
	}}}`)
	_, err := Resolve([]string{root})
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("Resolve = %v, want a *NamespaceError for a listed-but-missing agent", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q must name the missing entry", err.Error())
	}
}

// Same failure for a listed workflow shape whose workflows/<name>.yaml is absent.
func TestResolve_ManifestListedWorkflowMissingFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"workflows":["ghost-job"]
	}}}`)
	_, err := Resolve([]string{root})
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("Resolve = %v, want a *NamespaceError for a listed-but-missing workflow", err)
	}
	if !strings.Contains(err.Error(), "ghost-job") {
		t.Errorf("error %q must name the missing entry", err.Error())
	}
}

// An agents/ bundle present on disk but absent from the manifest's list is
// not an error - Resolve succeeds, and it is simply excluded from p.Agents
// (server_validate_test.go/plugin_agents_test.go cover the resulting "not
// seeded" behavior; the warning itself isn't observable through Resolve's return value).
func TestResolve_PresentButUnlistedAgentDoesNotFail(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"agents":["scout"]
	}}}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)
	writeFile(t, filepath.Join(root, "agents", "extra", "agent-card.json"), `{"name":"extra"}`)

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got[0].Agents) != 1 || got[0].Agents[0] != "scout" {
		t.Errorf("Agents = %v, want [scout] (the unlisted \"extra\" bundle must not be added)", got[0].Agents)
	}
}

// A namespace block that omits the agents/workflows keys entirely leaves
// Plugin.Agents/Workflows nil - nothing seeds from AgentsDir/WorkflowsDir,
// same as an explicit empty list, and every present bundle still gets the
// unlisted warning (proven via a captured log below).
func TestResolve_ManifestListsOmittedLeaveNil(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{"schemaVersion":1}}}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[0].Agents != nil {
		t.Errorf("Agents = %v, want nil (key omitted)", got[0].Agents)
	}
	if !strings.Contains(buf.String(), "scout") || !strings.Contains(buf.String(), "not listed") {
		t.Errorf("log output %q must warn about the unlisted scout bundle even with the key omitted", buf.String())
	}
}

// Two plugins listing the same agent name fail with a NamespaceError-class
// error naming both plugins and the entry, instead of silently merging into
// one mixed agent.
func TestResolve_CrossPluginAgentCollisionFails(t *testing.T) {
	a := manifest(t, `{"$schema":"x","name":"a","extensions":{"`+Namespace+`":{"schemaVersion":1,"agents":["scout"]}}}`)
	writeFile(t, filepath.Join(a, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)
	b := manifest(t, `{"$schema":"x","name":"b","extensions":{"`+Namespace+`":{"schemaVersion":1,"agents":["scout"]}}}`)
	writeFile(t, filepath.Join(b, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)

	_, err := Resolve([]string{a, b})
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("Resolve = %v, want a *NamespaceError for a cross-plugin collision", err)
	}
	if !strings.Contains(err.Error(), "\"a\"") || !strings.Contains(err.Error(), "\"b\"") || !strings.Contains(err.Error(), "scout") {
		t.Errorf("error %q must name both plugins and the entry", err.Error())
	}
}

// Same collision, for two plugins' workflows lists.
func TestResolve_CrossPluginWorkflowCollisionFails(t *testing.T) {
	a := manifest(t, `{"$schema":"x","name":"a","extensions":{"`+Namespace+`":{"schemaVersion":1,"workflows":["job"]}}}`)
	writeFile(t, filepath.Join(a, "workflows", "job.yaml"), "name: job")
	b := manifest(t, `{"$schema":"x","name":"b","extensions":{"`+Namespace+`":{"schemaVersion":1,"workflows":["job"]}}}`)
	writeFile(t, filepath.Join(b, "workflows", "job.yaml"), "name: job")

	_, err := Resolve([]string{a, b})
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("Resolve = %v, want a *NamespaceError for a cross-plugin collision", err)
	}
	if !strings.Contains(err.Error(), "\"a\"") || !strings.Contains(err.Error(), "\"b\"") || !strings.Contains(err.Error(), "job") {
		t.Errorf("error %q must name both plugins and the entry", err.Error())
	}
}
