package config

import "testing"

// judge: false must be parseable and distinct from unset (nil = judge on, false = off).
// A text judge (gemma) cannot evaluate a media transcription it never saw, so media/image
// readers set judge: false — the gate then skips the independent judge (JudgeRounds forced
// to 0 in serve.go; RunGatedRefine's loop is `round <= JudgeRounds`).
func TestAgentJudgeToggleParses(t *testing.T) {
	on := true
	off := false
	for name, ac := range map[string]AgentConfig{
		"unset": {},
		"on":    {Judge: &on},
		"off":   {Judge: &off},
	} {
		switch name {
		case "unset":
			if ac.Judge != nil {
				t.Errorf("unset judge should be nil (inherit default on), got %v", *ac.Judge)
			}
		case "off":
			if ac.Judge == nil || *ac.Judge {
				t.Errorf("judge:false must be an explicit false, got %v", ac.Judge)
			}
		}
	}
}
