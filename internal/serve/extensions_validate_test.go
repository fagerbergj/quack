package serve

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
)

func TestValidateExtensions(t *testing.T) {
	cases := []struct{ name, ext, wantErr, want string }{
		{"accepted", "noop: {greeting: hi}", "", "noop"},
		{"disabled skips factory", "usage: {enabled: false}", "", ""},
		{"factory rejects", "usage: {tempo_url: http://t}", "extensions.usage: factory: usage: prometheus_url is required", ""},
		{"not compiled", "nosuch: {x: 1}", "extensions.nosuch is not a compiled extension", ""},
		{"bad base config", "noop: {enabled: notabool}", "extensions.noop: parse base config", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			if err := yaml.Unmarshal([]byte(tc.ext), &cfg.Extensions.Modules); err != nil {
				t.Fatal(err)
			}
			got, err := ValidateExtensions(cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != tc.want {
				t.Fatalf("ValidateExtensions = %v, %v; want %q", got, err, tc.want)
			}
		})
	}
}
