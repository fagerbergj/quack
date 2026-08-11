package serve

// The blessed-extension list: quack's default image is batteries-included
// (design doc "Model"), so every first-party quack-extensions module quack
// ships is blank-imported here to register itself with the SDK. Compiled but
// unconfigured stays dormant; add a module by adding its import.
import (
	_ "github.com/fagerbergj/quack-extensions/github"
	_ "github.com/fagerbergj/quack-extensions/noop"
	_ "github.com/fagerbergj/quack-extensions/remarkable"
)
