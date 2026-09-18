package serve

// The blessed-extension list: quack's default image is batteries-included
// (design doc "Model"), so every first-party quack-extensions module quack
// ships is blank-imported here to register itself with the SDK. Compiled but
// unconfigured stays dormant; add a module by adding its import.
import (
	_ "github.com/fagerbergj/quack-extensions/github"
	_ "github.com/fagerbergj/quack-extensions/noop"
	_ "github.com/fagerbergj/quack-extensions/remarkable"
	_ "github.com/fagerbergj/quack-extensions/sleeper"
	_ "github.com/fagerbergj/quack-extensions/usage"

	// sleeperkinds registers the sleeper agent bundles' artifact kinds
	// unconditionally, so agent.LoadBundle validates them at boot whether or
	// not extensions.sleeper is enabled (see internal/sleeperkinds).
	_ "github.com/fagerbergj/quack/internal/sleeperkinds"
)
