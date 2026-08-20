package plugin

import (
	"path/filepath"
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
	if dirs := ResolveSkillDirs([]string{root}); len(dirs) != 0 {
		t.Fatalf("dirs = %v, want none", dirs)
	}
}
