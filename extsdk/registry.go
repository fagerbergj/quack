package sdk

// Registered returns the map of all registered extension factories.
// Populated by blank imports from individual extension modules (see
// extensions_registry.go in each consuming repo). Nil if no extensions
// are compiled in.
var registered = make(map[string]Factory)

// Register adds an extension factory to the registry. Called exactly once
// per module, via init() or explicit registration in a blank-import file.
func Register(name string, f Factory) {
	registered[name] = f
}

// Registered returns the full registry of compiled-in extensions.
func Registered() map[string]Factory { return registered }
