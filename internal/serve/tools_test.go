package serve

import (
	"reflect"
	"testing"
)

// TestResolveToolNames guards the config-driven gating of runtime-conditional
// builtins: stage_memory needs a task-memory store, ask_advisor needs a built
// advisor agent (itself gated on gates.judge being enabled - see build's
// advisorAgent). Both are silently dropped rather than erroring when their
// dependency is off, and load_memory is split out (ADK-native, added by the
// caller) regardless.
func TestResolveToolNames(t *testing.T) {
	cases := []struct {
		name             string
		configured       []string
		taskMemAvailable bool
		advisorAvailable bool
		wantNames        []string
		wantWantLoadMem  bool
	}{
		{
			name:             "ask_advisor present when advisor available",
			configured:       []string{"web_search", "ask_advisor"},
			advisorAvailable: true,
			wantNames:        []string{"web_search", "ask_advisor"},
		},
		{
			name:             "ask_advisor absent when advisor unavailable (JudgeEnabled=false)",
			configured:       []string{"web_search", "ask_advisor"},
			advisorAvailable: false,
			wantNames:        []string{"web_search"},
		},
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
			name:            "load_memory split out regardless of tool availability",
			configured:      []string{"load_memory", "web_search"},
			wantNames:       []string{"web_search"},
			wantWantLoadMem: true,
		},
		{
			name:       "unrelated tools always pass through",
			configured: []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
			wantNames:  []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
		},
		{
			// Extension tool names (internal/github.App.Tools()) are not special-cased
			// here - they resolve later, in tools.Build, against Deps.ExtTools. An
			// agent gets one only by listing it, same as any builtin.
			name:       "extension tool names pass through unchanged",
			configured: []string{"read_file", "github_add_review_comment"},
			wantNames:  []string{"read_file", "github_add_review_comment"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNames, gotWantLoadMem := resolveToolNames(tc.configured, tc.taskMemAvailable, tc.advisorAvailable)
			if !reflect.DeepEqual(gotNames, tc.wantNames) {
				t.Errorf("names = %v, want %v", gotNames, tc.wantNames)
			}
			if gotWantLoadMem != tc.wantWantLoadMem {
				t.Errorf("wantLoadMemory = %v, want %v", gotWantLoadMem, tc.wantWantLoadMem)
			}
		})
	}
}
