package vetting

import "testing"

// TestSameArgsJSONNormalization: the pinning comparison must survive JSON
// transport differences - an int pinned at approval time still matches the
// same value arriving as float64 on the re-issued call - while any real
// difference in values, keys, or nesting breaks the match.
func TestSameArgsJSONNormalization(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]any
		want bool
	}{
		{"identical", map[string]any{"branch": "x"}, map[string]any{"branch": "x"}, true},
		{"int vs float64", map[string]any{"depth": 5}, map[string]any{"depth": float64(5)}, true},
		{"nested int vs float64",
			map[string]any{"opts": map[string]any{"n": 2}},
			map[string]any{"opts": map[string]any{"n": float64(2)}}, true},
		{"nil vs empty", nil, map[string]any{}, true},
		{"different value", map[string]any{"branch": "feature-x"}, map[string]any{"branch": "main-mirror"}, false},
		{"different number", map[string]any{"depth": 1}, map[string]any{"depth": float64(2)}, false},
		{"extra key", map[string]any{"a": 1}, map[string]any{"a": float64(1), "force": true}, false},
		{"missing key", map[string]any{"a": 1, "b": 2}, map[string]any{"a": float64(1)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameArgs(c.a, c.b); got != c.want {
				t.Errorf("sameArgs(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
