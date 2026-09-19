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
// NamespaceError-class failure naming the entry - CheckManifestLists runs
// from internal/serve's admission path, not Resolve (#1430: a REST-added
// row's refusal must drop only that row, not brick boot).
func TestCheckManifestLists_ListedAgentMissingFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"agents":["ghost"]
	}}}`)
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = CheckManifestLists(got[0])
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("CheckManifestLists = %v, want a *NamespaceError for a listed-but-missing agent", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q must name the missing entry", err.Error())
	}
}

// A listed agent whose directory exists but has no agent-card.json is also
// listed-but-missing: "present" means "is a bundle", the same predicate
// config.SeedPluginAgents seeds by - not just "a directory with this name".
func TestCheckManifestLists_ListedAgentDirWithoutCardFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"agents":["scout"]
	}}}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "prompt.md"), "body") // no agent-card.json

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = CheckManifestLists(got[0])
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("CheckManifestLists = %v, want a *NamespaceError for a cardless bundle dir", err)
	}
	if !strings.Contains(err.Error(), "scout") {
		t.Errorf("error %q must name the missing entry", err.Error())
	}
}

// Same failure for a listed workflow shape whose workflows/<name>.yaml is absent.
func TestCheckManifestLists_ListedWorkflowMissingFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"workflows":["ghost-job"]
	}}}`)
	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = CheckManifestLists(got[0])
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("CheckManifestLists = %v, want a *NamespaceError for a listed-but-missing workflow", err)
	}
	if !strings.Contains(err.Error(), "ghost-job") {
		t.Errorf("error %q must name the missing entry", err.Error())
	}
}

// A workflows/<stem>.yaml whose internal name: field does not match its
// filename stem fails: listing, dedupe, seeding, and collision detection all
// key on the same string, so a mismatch would let a manifest list one name
// and silently seed a shape under another.
func TestCheckManifestLists_WorkflowNameMismatchFails(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"workflows":["alpha"]
	}}}`)
	writeFile(t, filepath.Join(root, "workflows", "alpha.yaml"), "name: not-alpha")

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	err = CheckManifestLists(got[0])
	var nsErr *NamespaceError
	if !errors.As(err, &nsErr) {
		t.Fatalf("CheckManifestLists = %v, want a *NamespaceError for a stem/name mismatch", err)
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error %q must name the file", err.Error())
	}
}

// An agents/ bundle present on disk but absent from the manifest's list is
// not an error - CheckManifestLists passes, and it is simply excluded from p.Agents.
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
	if err := CheckManifestLists(got[0]); err != nil {
		t.Fatalf("CheckManifestLists: %v", err)
	}
	if len(got[0].Agents) != 1 || got[0].Agents[0] != "scout" {
		t.Errorf("Agents = %v, want [scout] (the unlisted \"extra\" bundle must not be added)", got[0].Agents)
	}
}

// A namespace block that omits the agents/workflows keys entirely leaves
// Plugin.Agents/Workflows nil - nothing seeds from AgentsDir/WorkflowsDir,
// same as an explicit empty list, and every present bundle still gets the
// unlisted warning.
func TestResolve_ManifestListsOmittedLeaveNil(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{"schemaVersion":1}}}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[0].Agents != nil {
		t.Errorf("Agents = %v, want nil (key omitted)", got[0].Agents)
	}
	assertUnlistedWarning(t, got[0], "scout")
}

// The unlisted-bundle warning fires even when the plugin has no quack
// namespace block at all - an absent block means empty lists, not "seed
// everything present", so this can never be a silent regression to the old
// discover-by-presence behavior.
func TestWarnUnlistedManifestEntries_NoNamespaceBlockStillWarns(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p"}`)
	writeFile(t, filepath.Join(root, "agents", "scout", "agent-card.json"), `{"name":"scout"}`)

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[0].Agents != nil {
		t.Errorf("Agents = %v, want nil (no namespace block)", got[0].Agents)
	}
	assertUnlistedWarning(t, got[0], "scout")
}

// assertUnlistedWarning captures slog output around WarnUnlistedManifestEntries(p)
// and asserts it names wantName as present-but-unlisted.
func assertUnlistedWarning(t *testing.T, p Plugin, wantName string) {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	WarnUnlistedManifestEntries(p)
	if !strings.Contains(buf.String(), wantName) || !strings.Contains(buf.String(), "not listed") {
		t.Errorf("log output %q must warn about the unlisted %q bundle", buf.String(), wantName)
	}
}
