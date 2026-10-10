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

// §6.2: a plugin with no skills/ is legal, so a module-only or MCP-only plugin must still load.
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

// §8: a client must ignore namespaces it does not implement, without validating their contents.
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

// Inside our own namespace, a block declaring compiled-in code is the one failure quack never downgrades
// to a warning.
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

// A listed agent with no agents/<name>/ dir fails, naming the entry. CheckManifestLists runs from serve's
// admission path so a refusal drops only that row.
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

// A listed agent dir without agent-card.json is also missing: "present" means "is a bundle", the predicate
// config.SeedPluginAgents uses.
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

// A workflow whose name: differs from its filename stem fails: everything keys on one string, so a mismatch
// would seed a shape under an unlisted name.
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

// An unlisted workflow yaml that mismatches or fails to parse is warned about, not fatal: only listed
// entries' contracts are enforced.
func TestCheckManifestLists_UnlistedWorkflowMismatchOrMalformedDoesNotFail(t *testing.T) {
	root := manifest(t, `{"$schema":"x","name":"p","extensions":{"`+Namespace+`":{
		"schemaVersion":1,"workflows":["alpha"]
	}}}`)
	writeFile(t, filepath.Join(root, "workflows", "alpha.yaml"), "name: alpha")
	writeFile(t, filepath.Join(root, "workflows", "mismatched.yaml"), "name: something-else")
	writeFile(t, filepath.Join(root, "workflows", "broken.yaml"), "name: [unterminated")

	got, err := Resolve([]string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := CheckManifestLists(got[0]); err != nil {
		t.Fatalf("CheckManifestLists = %v, want no error - only alpha is listed", err)
	}
	if len(got[0].Workflows) != 1 || got[0].Workflows[0] != "alpha" {
		t.Errorf("Workflows = %v, want [alpha]", got[0].Workflows)
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

// Omitted agents/workflows keys leave Plugin.Agents/Workflows nil (same as empty), and every present
// bundle still gets the unlisted warning.
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

// The unlisted-bundle warning fires with no namespace block at all: an absent block means empty lists,
// not "seed everything present".
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
