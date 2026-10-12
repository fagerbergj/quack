package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/internal/serve"
)

// Every linked extension accepts a config, and every extensions/*/plugin seeds exactly the agents and workflows
// its manifest lists: the check a plugin/<name> tag cut from this tree relies on.
func TestServerValidate_EveryExtensionAndPlugin(t *testing.T) {
	tmp := t.TempDir()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_TEST_TMP", tmp)
	t.Setenv("QUACK_TEST_REPO", repo)
	t.Setenv("QUACK_RESEARCHER_MODEL", "researcher")
	t.Setenv("QUACK_TEST_SECRET", "placeholder")
	writeTestRSAKey(t, filepath.Join(tmp, "github.pem"))
	manifests, err := filepath.Glob(filepath.Join(repo, "extensions", "*", "plugin", "plugin.json"))
	if err != nil || len(manifests) == 0 {
		t.Fatalf("no extensions/*/plugin/plugin.json: %v", err)
	}
	for _, m := range manifests {
		name := filepath.Base(filepath.Dir(filepath.Dir(m)))
		if err := os.CopyFS(filepath.Join(tmp, "plugins", name), os.DirFS(filepath.Dir(m))); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	c := newServerValidateCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"testdata/every-extension.yaml", "--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("server validate --json: %v\n%s", err, out.String())
	}
	var got serverValidateResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	for name := range extsdk.Registered() {
		if !slices.Contains(got.Extensions, name) {
			t.Errorf("extension %s: not accepted (add an extensions.%s block to testdata/every-extension.yaml); accepted %v", name, name, got.Extensions)
		}
	}
	for _, m := range manifests {
		checkPluginSeeded(t, filepath.Base(filepath.Dir(filepath.Dir(m))), m, got)
	}
}

// checkPluginSeeded: a plugin seeds only what its manifest lists, so anything short of the lists means quack
// refused or skipped part of it.
func checkPluginSeeded(t *testing.T, name, manifest string, got serverValidateResult) {
	t.Helper()
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Extensions map[string]struct {
			Agents    []string `json:"agents"`
			Workflows []string `json:"workflows"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", manifest, err)
	}
	want := m.Extensions["io.github.fagerbergj.quack"]
	i := slices.IndexFunc(got.Plugins, func(p serve.PluginSeedResult) bool { return p.Plugin == name })
	if i < 0 {
		if len(want.Agents)+len(want.Workflows) > 0 {
			t.Errorf("plugin %s: not seeded; validate listed %+v", name, got.Plugins)
		}
		return
	}
	p := got.Plugins[i]
	if !sameSet(p.Agents, want.Agents) || !sameSet(p.Shapes, want.Workflows) {
		t.Errorf("plugin %s: manifest lists agents %v workflows %v, quack seeded agents %v shapes %v", name, want.Agents, want.Workflows, p.Agents, p.Shapes)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func writeTestRSAKey(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
}
