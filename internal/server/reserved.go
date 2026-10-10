package server

import (
	"fmt"
	"regexp"
)

// ReservedRouteNames are top-level segments quack's routes and the SPA router (frontend/src/router.ts) claim;
// an extension mounted at one would shadow or be shadowed. Extend it with every new top-level route.
var ReservedRouteNames = []string{
	"api", "assets", "chat", "debug", "ext", "health", "healthz", "memory", "static",
}

// extensionNamePattern: lowercase letters, digits, and single dashes between
// them - what's safe to use as a literal chi mount path segment.
var extensionNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateExtensionName rejects a name that isn't URL-safe or is reserved. Run it at startup for every
// extension actually mounted, not every compiled-in name.
func ValidateExtensionName(name string) error {
	if !extensionNamePattern.MatchString(name) {
		return fmt.Errorf("extension name %q must be lowercase alphanumeric with single dashes between segments", name)
	}
	for _, reserved := range ReservedRouteNames {
		if name == reserved {
			return fmt.Errorf("extension name %q collides with reserved route name %q", name, reserved)
		}
	}
	return nil
}
