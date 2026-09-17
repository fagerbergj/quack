package memory

import "github.com/fagerbergj/quack/internal/memoryrules"

// Aliases onto internal/memoryrules (moved there to break an import cycle: internal/config needs
// the parser too, and internal/memory already imports internal/config). Kept here so every
// existing internal/memory call site is unchanged.
type Rule = memoryrules.Rule
type Fields = memoryrules.Fields

const (
	ThenInvalidate = memoryrules.ThenInvalidate
	ThenDemote     = memoryrules.ThenDemote
	ThenKeep       = memoryrules.ThenKeep

	ReasonNeverRecalled          = memoryrules.ReasonNeverRecalled
	ReasonRecalledWithoutSupport = memoryrules.ReasonRecalledWithoutSupport
)

var (
	DefaultRules  = memoryrules.DefaultRules
	ValidateRules = memoryrules.ValidateRules
	Evaluate      = memoryrules.Evaluate
)
