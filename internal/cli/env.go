package cli

import "os"

// PrefillFromEnv seeds an InitAnswers from the conventional environment so the
// wizard doesn't re-ask what's already set. The shipped config + docker-compose
// use these names, so a user with .env sourced gets every field pre-filled and
// just confirms. Empty env vars leave the field blank (the wizard's heuristics
// or manual entry fill those). The mapping:
//
//	LLM_ENDPOINT  → endpoint
//	LLM_API_KEY   → api key
//	ORCH_MODEL    → main model (RESEARCHER_MODEL as a fallback)
//	JUDGE_MODEL   → judge
//	EMBED_MODEL   → embedder
//	IMAGE_MODEL   → vision
//	MEDIA_MODEL   → audio
func PrefillFromEnv(a *InitAnswers) {
	a.Endpoint = os.Getenv("LLM_ENDPOINT")
	a.APIKey = os.Getenv("LLM_API_KEY")
	if a.MainModel == "" {
		a.MainModel = os.Getenv("ORCH_MODEL")
		if a.MainModel == "" {
			a.MainModel = os.Getenv("RESEARCHER_MODEL")
		}
	}
	if a.JudgeModel == "" {
		a.JudgeModel = os.Getenv("JUDGE_MODEL")
	}
	if a.EmbedModel == "" {
		a.EmbedModel = os.Getenv("EMBED_MODEL")
	}
	if a.VisionModel == "" {
		a.VisionModel = os.Getenv("IMAGE_MODEL")
	}
	if a.AudioModel == "" {
		a.AudioModel = os.Getenv("MEDIA_MODEL")
	}
}
