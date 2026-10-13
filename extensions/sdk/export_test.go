package sdk

// Unregister lets tests drop a name, since the registry outlives one -count run.
func Unregister(name string) { delete(registry, name) }
