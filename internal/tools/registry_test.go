package tools

import (
	"reflect"
	"slices"
	"testing"
)

// TestResolveNames guards the config-driven gating of runtime-conditional builtins,
// dropped silently rather than erroring - load_memory is gated exactly like recall_memory.
func TestResolveNames(t *testing.T) {
	cases := []struct {
		name             string
		configured       []string
		taskMemAvailable bool
		wantNames        []string
	}{
		{
			name:             "stage_memory present when task memory available",
			configured:       []string{"stage_memory"},
			taskMemAvailable: true,
			wantNames:        []string{"stage_memory"},
		},
		{
			name:             "stage_memory absent when task memory unavailable",
			configured:       []string{"stage_memory"},
			taskMemAvailable: false,
			wantNames:        []string{},
		},
		{
			name:             "load_memory present when task memory available",
			configured:       []string{"load_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"load_memory", "web_search"},
		},
		{
			name:             "load_memory absent when task memory unavailable",
			configured:       []string{"load_memory", "web_search"},
			taskMemAvailable: false,
			wantNames:        []string{"web_search"},
		},
		{
			name:             "load_memory then recall_memory collapses to the first",
			configured:       []string{"load_memory", "recall_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"load_memory", "web_search"},
		},
		{
			name:             "recall_memory then load_memory collapses to the first",
			configured:       []string{"recall_memory", "load_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"recall_memory", "web_search"},
		},
		{
			name:       "unrelated tools always pass through",
			configured: []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
			wantNames:  []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
		},
		{
			name:       "native artifact tools are per-dispatch, not registry builds",
			configured: []string{"read_artifact", "write_artifact", "check_mermaid"},
			wantNames:  []string{"check_mermaid"},
		},
		{
			// Extension tool names aren't special-cased: they resolve later in tools.Build against
			// Deps.ExtTools, and an agent gets one only by listing it.
			name:       "extension tool names pass through unchanged",
			configured: []string{"read_file", "github_add_review_comment"},
			wantNames:  []string{"read_file", "github_add_review_comment"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNames := ResolveNames(tc.configured, tc.taskMemAvailable)
			if !reflect.DeepEqual(gotNames, tc.wantNames) {
				t.Errorf("names = %v, want %v", gotNames, tc.wantNames)
			}
		})
	}
}

func TestBuiltinNamesSorted(t *testing.T) {
	names := BuiltinNames()
	if len(names) != len(registry) || !slices.IsSorted(names) {
		t.Fatalf("BuiltinNames() = %v, want every registry key, sorted", names)
	}
}
