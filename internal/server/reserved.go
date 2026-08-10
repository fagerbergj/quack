package server

import (
	"fmt"
	"regexp"
)

// ReservedRouteNames are top-level path segments quack's own routes and the
// SPA's client-side router already claim: "api" (REST/MCP), "debug"
// (adkdebug.MountPath), "health"/"healthz" (liveness), "assets" (Vite's
// default build-output dir), "ext" (reserved as a namespace word, not used
// as a path prefix itself), and the SPA's own routes from
// frontend/src/router.ts ("chat", "memory"). "static" is reserved
// defensively for the same reason as "assets". An SDK extension mounted at
// one of these would shadow, or be shadowed by, a route quack already owns.
// Extend this list whenever a new top-level SPA route or server-owned path
// segment is added.
var ReservedRouteNames = []string{
	"api", "assets", "chat", "debug", "ext", "health", "healthz", "memory", "static",
}

// extensionNamePattern: lowercase letters, digits, and single dashes between
// them - what's safe to use as a literal chi mount path segment.
var extensionNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateExtensionName rejects a name that isn't URL-safe or collides with
// a ReservedRouteNames entry. Callers should run this once at startup for
// every extension that will actually be mounted (registered AND
// configured), not every compiled-in name.
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
