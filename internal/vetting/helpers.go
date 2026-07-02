package vetting

import (
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
)

// Config carries the per-agent trust-gate settings consumed by RunGatedRefine
// (the native refine loop) and the independent judge (judge.go). Built from
// config via FromConfig (rubric.go), with a per-agent rubric override applied by
// the caller.
type Config struct {
	DeterministicRounds int     // cheap citation/length check + targeted revise cycles
	SelfCritiqueRounds  int     // (legacy) worker self-improvement passes; dropped in v2 (advisor replaces)
	JudgeRounds         int     // expensive model-judge/revise rounds
	Threshold           float64 // judge pass score in (0,1]
	JudgeMaxIterations  int     // cap on the agentic judge's model turns per round (0 ⇒ default)
	Constitution        string  // global principles; prefixed in the judge prompt
	Rubric              string  // scoring guide; global default or per-agent override

	// Memory, when set, receives the agent's staged tradecraft on a judge pass
	// (M6). nil disables the gated commit path.
	// ponytail: the gated-commit-on-pass path is not yet wired into RunGatedRefine
	// (dropped with the custom-agent gate); re-add in a memory follow-up.
	Memory *memory.Store
	// CommitMemory marks this agent as a task-memory participant.
	CommitMemory bool
}

// maxEmptyRetries bounds the empty-answer recovery re-invocations in RunGatedRefine.
const maxEmptyRetries = 4

// fetchSampleBytes is how many bytes of fetched content we keep per URL — enough
// for the judge to spot-check a claim, small enough not to flood its context.
const fetchSampleBytes = 300

// fetchRecord is the retained sample of a fetched page, for judge spot-checking.
type fetchRecord struct {
	sample string
}

// workerActivity summarises the worker's retrieval (reconstructed from session
// events by activityFromSession). Passed to the judge + deterministic citation
// check so neither can falsely claim no retrieval happened.
type workerActivity struct {
	searches []string               // every web_search query
	fetched  map[string]fetchRecord // URL → sample for web_fetch calls that returned content
	seen     map[string]string      // URL → search snippet for surfaced-but-not-fetched URLs
	staged   []memory.Candidate     // memory candidates staged via stage_memory (M6)
}

// contentPlainText concatenates the plain-text parts of a content.
func contentPlainText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// recordSearchResults extracts {url: snippet} pairs from a web_search response
// (shape {results: [{title, url, snippet}]}) into seen. Each surfaced URL is a
// genuinely-retrieved lead — a valid source if later cited. First snippet wins.
func recordSearchResults(seen map[string]string, resp map[string]any) {
	if resp == nil {
		return
	}
	var items []any
	switch r := resp["results"].(type) {
	case []any:
		items = r
	case []map[string]any:
		items = make([]any, len(r))
		for i, m := range r {
			items[i] = m
		}
	default:
		return
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		snippet, _ := m["snippet"].(string)
		seen[u] = strings.TrimSpace(trimToSample(snippet))
	}
}

// trimToSample truncates s to fetchSampleBytes at a valid UTF-8 boundary.
func trimToSample(s string) string {
	if len(s) <= fetchSampleBytes {
		return s
	}
	s = s[:fetchSampleBytes]
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}
