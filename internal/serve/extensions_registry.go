package serve

// Blank-imports every first-party extension module so it registers with the SDK; compiled but
// unconfigured modules stay dormant.
import (
	_ "github.com/fagerbergj/quack-extensions/github"
	_ "github.com/fagerbergj/quack-extensions/noop"
	_ "github.com/fagerbergj/quack-extensions/remarkable"
	_ "github.com/fagerbergj/quack-extensions/sleeper"
	_ "github.com/fagerbergj/quack-extensions/usage"

	// Registers sleeper bundles' artifact kinds unconditionally so agent.LoadBundle validates them at boot
	// whether or not extensions.sleeper is enabled.
	_ "github.com/fagerbergj/quack/internal/sleeperkinds"
)
