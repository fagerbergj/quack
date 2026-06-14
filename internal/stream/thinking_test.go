package stream

import "testing"

func TestStripThinking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"closed block", "<think>weighing options</think>\n\nThe final answer", "The final answer"},
		{"bare closing tag", "</think>\nThe answer", "The answer"},
		{"unclosed leading block", "<think>reasoning that never closes because the budget ran out", ""},
		{"unclosed after some answer", "Partial answer.<think>now rambling with no close", "Partial answer."},
		{"no markers", "Just a clean answer", "Just a clean answer"},
		{"closed then trailing whitespace", "<think>x</think>   Answer  ", "Answer"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripThinking(c.in); got != c.want {
				t.Errorf("StripThinking(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
