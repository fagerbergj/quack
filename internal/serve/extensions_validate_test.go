package serve

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
)

func extensionsConfig(t *testing.T, root, modules string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Workspace.Root = root
	if err := yaml.Unmarshal([]byte(modules), &cfg.Extensions.Modules); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestValidateExtensions(t *testing.T) {
	cases := []struct{ name, ext, wantErr, accepted, disabled string }{
		{"accepted", "noop: {greeting: hi}", "", "noop", ""},
		{"disabled skips factory", "github: {enabled: false}", "", "", "github"},
		{"factory rejects", "usage: {tempo_url: http://t}", "extensions.usage: factory:", "", ""},
		{"not compiled", "nosuch: {x: 1}", "extensions.nosuch is not a compiled extension", "", ""},
		{"bad base config", "noop: {enabled: notabool}", "extensions.noop: parse base config", "", ""},
		{"data dir not creatable", "noop: {data_dir: $DIR/file/sub}", "extensions.noop: data dir:", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ValidateExtensions(extensionsConfig(t, dir, strings.ReplaceAll(tc.ext, "$DIR", dir)))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got.Accepted, ",") != tc.accepted || strings.Join(got.Disabled, ",") != tc.disabled {
				t.Fatalf("ValidateExtensions = %+v, %v; want accepted %q disabled %q", got, err, tc.accepted, tc.disabled)
			}
		})
	}
}

// Every failing extension is reported, not just the first.
func TestValidateExtensions_JoinsErrors(t *testing.T) {
	_, err := ValidateExtensions(extensionsConfig(t, t.TempDir(), "usage: {tempo_url: http://t}\nnosuch: {x: 1}\nnoop: {}"))
	for _, want := range []string{"extensions.usage: factory:", "extensions.nosuch is not a compiled extension"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
}

// github's Factory opens its store in DataDir; validate must hand it a throwaway
// dir and leave both a configured data_dir and the workspace default untouched.
func TestValidateExtensions_LeavesRealDataDirsUntouched(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "workspace")
	noopDir := filepath.Join(dir, "noop-data")
	cfg := extensionsConfig(t, root, githubModule(t, dir)+"\nnoop: {data_dir: "+noopDir+"}")
	got, err := ValidateExtensions(cfg)
	if err != nil || strings.Join(got.Accepted, ",") != "github,noop" {
		t.Fatalf("ValidateExtensions = %+v, %v; want github,noop accepted", got, err)
	}
	for _, p := range []string{root, noopDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after validate (stat err %v); validate must not create or touch real data dirs", p, err)
		}
	}
}

// githubModule is an extensions.github block whose Factory accepts it offline.
func githubModule(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return "github: {client_id: Iv1.x, private_key_path: " + keyPath + ", webhook_secret: s}"
}

// validate checks decisions.points against the enabled extensions' declarations, as boot does.
func TestValidateExtensions_ChecksDecisionPoints(t *testing.T) {
	dir := t.TempDir()
	github := githubModule(t, dir)
	for _, c := range []struct{ name, modules, point, want string }{
		{"extension not enabled", "noop: {}", "ext:github/finding.severity", `extension "github" is not enabled (enabled: noop)`},
		{"extension declares nothing", "noop: {}", "ext:noop/start_sit", `extension "noop" declares no decision points`},
		// The prefix only: the real module's point list grows with each release.
		{"undeclared name", github, "ext:github/finding.severty", `extension "github" declares no such point (declared: ext:github/`},
		{"declared", github, "ext:github/finding.severity", ""},
	} {
		cfg := extensionsConfig(t, filepath.Join(dir, "workspace"), c.modules)
		cfg.Decisions = config.DecisionsConfig{Handlers: map[string]config.DecisionHandler{"clef": {URL: "http://x"}},
			Points: map[string]config.DecisionPoint{c.point: {Enabled: true, Handler: "clef", Mode: "observe"}}}
		_, err := ValidateExtensions(cfg)
		if (c.want == "" && err != nil) || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
