package sdk

import (
	"fmt"
	"maps"
)

var registry = map[string]Factory{}

// Register is called from an extension package's init(). A duplicate name is a
// programmer error, so it panics at init rather than failing at startup.
func Register(name string, f Factory) {
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("sdk: extension %q already registered", name))
	}
	registry[name] = f
}

// Registered returns a copy of all registered factories, keyed by name.
func Registered() map[string]Factory {
	return maps.Clone(registry)
}
