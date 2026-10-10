package memoryrules

import "testing"

func TestDefaultRules(t *testing.T) {
	t.Parallel()
	rules := DefaultRules()
	cases := []struct {
		name string
		rule int
		f    Fields
		want bool
	}{
		{"rule 0 never recalled", 0, Fields{Tier: "unverified", DaysSinceMinted: 31}, true},
		{"rule 0 boundary", 0, Fields{Tier: "unverified", DaysSinceMinted: 30}, false},
		{"rule 0 guarded by supported", 0, Fields{Tier: "unverified", Supported: 1, DaysSinceMinted: 31}, false},
		{"rule 0 guarded by recalls", 0, Fields{Tier: "unverified", Recalls: 1, DaysSinceMinted: 31}, false},
		{"rule 1 recalled without support", 1, Fields{Tier: "unverified", Recalls: 3}, true},
		{"rule 1 guarded by supported", 1, Fields{Tier: "unverified", Recalls: 3, Supported: 1}, false},
		{"rule 1 verified exempt", 1, Fields{Tier: "verified", Recalls: 3}, false},
		{"rule 2 bad score", 2, Fields{Score: -3}, true},
		{"rule 2 boundary", 2, Fields{Score: -2}, true},
		{"rule 2 above", 2, Fields{Score: -1}, false},
		{"rule 3 support decayed", 3, Fields{Tier: "verified", DaysSinceUpvote: 91}, true},
		{"rule 3 boundary", 3, Fields{Tier: "verified", DaysSinceUpvote: 90}, false},
		{"rule 4 keep verified", 4, Fields{Tier: "verified"}, true},
		{"rule 4 unverified", 4, Fields{Tier: "unverified"}, false},
	}
	for _, c := range cases {
		if got := rules[c.rule].Match(c.f); got != c.want {
			t.Errorf("%s: Match(%+v) = %v, want %v", c.name, c.f, got, c.want)
		}
	}
}
