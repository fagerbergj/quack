// Package memoryrules holds the built-in forgetting rules the nightly memory sweep applies.
package memoryrules

// Rule is one forgetting rule: Match decides, Then is invalidate|demote|keep (first match wins, none
// keeps). When labels the rule in sweep reports and in invalidate's default reason "rule N: <When>".
type Rule struct {
	When   string
	Then   string
	Reason string
	Match  func(Fields) bool
}

const (
	ThenInvalidate = "invalidate"
	ThenDemote     = "demote"
	ThenKeep       = "keep"
)

// Fields is one memory's forgetting-relevant snapshot; DaysSinceUpvote falls back to the memory's age
// when never upvoted, so such a memory ages out on age alone.
type Fields struct {
	Supported       int
	Score           int
	Recalls         int
	DaysSinceUpvote int64
	DaysSinceMinted int64
	Tier            string
}

// ReasonNeverRecalled/ReasonRecalledWithoutSupport: invalidation_reasons DefaultRules' usage rules stamp.
const (
	ReasonNeverRecalled          = "never recalled"
	ReasonRecalledWithoutSupport = "recalled without support"
)

// DefaultRules are the sweep's rules. Demote resets tier only, leaving supported intact, so the next
// vote or consolidator write recomputes tier from it.
func DefaultRules() []Rule {
	return []Rule{
		{When: `tier == "unverified" && supported == 0 && recalls == 0 && days_since_minted > 30`, Then: ThenInvalidate, Reason: ReasonNeverRecalled,
			Match: func(f Fields) bool {
				return f.Tier == "unverified" && f.Supported == 0 && f.Recalls == 0 && f.DaysSinceMinted > 30
			}},
		{When: `tier == "unverified" && recalls >= 3 && supported == 0`, Then: ThenInvalidate, Reason: ReasonRecalledWithoutSupport,
			Match: func(f Fields) bool { return f.Tier == "unverified" && f.Recalls >= 3 && f.Supported == 0 }},
		{When: `score <= -2`, Then: ThenInvalidate,
			Match: func(f Fields) bool { return f.Score <= -2 }},
		{When: `tier == "verified" && days_since_upvote > 90`, Then: ThenDemote,
			Match: func(f Fields) bool { return f.Tier == "verified" && f.DaysSinceUpvote > 90 }},
		{When: `tier == "verified"`, Then: ThenKeep,
			Match: func(f Fields) bool { return f.Tier == "verified" }},
	}
}
