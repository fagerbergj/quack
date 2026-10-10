package serve

import (
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
)

// TestSDKExtensionRegistryHasBlessedModules: every module blank-imported in extensions_registry.go
// registered itself with the SDK.
func TestSDKExtensionRegistryHasBlessedModules(t *testing.T) {
	factories := extsdk.Registered()
	for _, name := range []string{"noop", "remarkable", "sleeper", "usage"} {
		if _, ok := factories[name]; !ok {
			t.Errorf("extsdk.Registered() missing %q; want extensions_registry.go's blank import to have registered it", name)
		}
	}
}
