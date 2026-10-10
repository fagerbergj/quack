package config

import "testing"

// judge: false must parse distinct from unset (nil = on): media/image readers set it, since a text
// judge cannot evaluate a transcription of media it never saw.
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
