package serve

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// server.ui: none hands the router no SPA (it 404s SPA paths); the default embeds it.
func TestEmbeddedSPAFollowsServerUI(t *testing.T) {
	for _, tc := range []struct {
		ui      string
		wantSPA bool
	}{{"", true}, {config.UIEmbedded, true}, {config.UINone, false}} {
		spa, err := embeddedSPA(config.ServerConfig{UI: tc.ui})
		if err != nil {
			t.Fatalf("ui %q: %v", tc.ui, err)
		}
		if (spa != nil) != tc.wantSPA {
			t.Errorf("ui %q: SPA present = %v, want %v", tc.ui, spa != nil, tc.wantSPA)
		}
	}
}
