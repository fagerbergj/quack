package cli

import "os"

// PrefillFromEnv seeds an InitAnswers from the conventional environment so the
// wizard doesn't re-ask what's already set. The shipped config + docker-compose
// use these names, so a user with .env sourced gets every field pre-filled and
// just confirms. Empty env vars leave the field blank (the wizard's heuristics
// or manual entry fill those). The mapping:
//
//	QUACK_LLM_ENDPOINT  → endpoint
//	QUACK_LLM_API_KEY   → api key
//	QUACK_ORCH_MODEL    → main model (QUACK_RESEARCHER_MODEL as a fallback)
//	QUACK_JUDGE_MODEL   → judge
//	QUACK_EMBED_MODEL   → embedder
//	QUACK_IMAGE_MODEL   → vision
//	QUACK_MEDIA_MODEL   → audio
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
