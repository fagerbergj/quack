package cli

import "os"

// PrefillFromEnv seeds an InitAnswers from the conventional environment (the
// shipped config + docker-compose use these same names) so the wizard doesn't
// re-ask what's already set. Empty env vars leave the field blank for the
// wizard's heuristics or manual entry to fill.
func PrefillFromEnv(a *InitAnswers) {
	a.Endpoint = os.Getenv("QUACK_LLM_ENDPOINT")
	a.APIKey = os.Getenv("QUACK_LLM_API_KEY")
	if a.MainModel == "" {
		a.MainModel = os.Getenv("QUACK_ORCH_MODEL")
		if a.MainModel == "" {
			a.MainModel = os.Getenv("QUACK_RESEARCHER_MODEL")
		}
	}
	if a.JudgeModel == "" {
		a.JudgeModel = os.Getenv("QUACK_JUDGE_MODEL")
	}
	if a.EmbedModel == "" {
		a.EmbedModel = os.Getenv("QUACK_EMBED_MODEL")
	}
	if a.VisionModel == "" {
		a.VisionModel = os.Getenv("QUACK_IMAGE_MODEL")
	}
	if a.AudioModel == "" {
		a.AudioModel = os.Getenv("QUACK_MEDIA_MODEL")
	}
}
