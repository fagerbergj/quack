package sdk_test

import (
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"gopkg.in/yaml.v3"
)

func TestBaseConfigUnmarshalsReservedKeys(t *testing.T) {
	var cfg sdk.BaseConfig
	if err := yaml.Unmarshal([]byte("enabled: false\ndata_dir: /var/lib/quack/ext\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Enabled == nil || *cfg.Enabled != false {
		t.Errorf("Enabled = %v, want false", cfg.Enabled)
	}
	if cfg.DataDir != "/var/lib/quack/ext" {
		t.Errorf("DataDir = %q, want /var/lib/quack/ext", cfg.DataDir)
	}
}

func TestBaseConfigEnabledNilWhenAbsent(t *testing.T) {
	var cfg sdk.BaseConfig
	if err := yaml.Unmarshal([]byte("greeting: hi\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Enabled != nil {
		t.Errorf("Enabled = %v, want nil (absent key)", *cfg.Enabled)
	}
}
