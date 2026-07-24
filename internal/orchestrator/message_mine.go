package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/memory"
)

// preferenceRule matches one class of user-stated preferences and produces a
// Candidate for commit. The pattern is case-insensitive; content normalises to
// third-person present-tense declarative ("User prefers..."), making each fact
// independent of the original phrasing so duplicate detection stays reliable.
type preferenceRule struct {
	pattern *regexp.Regexp // positive match
	exclude *regexp.Regexp // negative filter (runs after match)
	content string         // normalised commit text
	kind    string         // preference|goal|limit
}

var preferenceRules = []preferenceRule{
	// --- Verbosity ---
	{
		pattern: regexp.MustCompile(`(?i)\b(be\s+(?:concise|terse|brief\b)|(?:keep\s*(?:it\s*)?|respond|reply|comment|write)\s*(?:it\s*)?\s*(terse\b|concise\b|brief\b|short\b|less verbose\b|very brief\b|no long answer)\b)`),
		content: "User prefers concise, terse responses — short answers without unnecessary detail.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)\b(?:keep\s*(?:it\s*)?|be)\s+(detailed\b|thorough\b|long\b|verbose\b|elaborate\b|give details\b|explain in detail)\b`),
		content: "User prefers detailed, thorough responses with full explanations.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)(no\s+fluff\b|cut\s+the\w*\s+(chatter|pleasantries|fluff)|skip\s+(introductory\b|small\s?talk))`),
		content: "User prefers no fluff — get straight to the answer.",
		kind:    "preference",
	},

	// --- PR/Branch style ---
	{
		pattern: regexp.MustCompile(`(?i)(?:always\s+)?(?:open|create)\s+(?:a\s+)?(?:pull\s*request|PR\b)\b`),
		content: "User wants branches automatically opened as pull requests.",
		kind:    "preference",
		exclude: regexp.MustCompile(`(?i)do not open|don't open|want.*branch only`),
	},
	{
		pattern: regexp.MustCompile(`(?i)(stay on a ?branch|keep (?:the )?code on a ?branch|work on a ?branch|don't open an? PR|do not open an? PR|no PRs?\b)`),
		content: "User wants work kept on a branch — do not open a pull request.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)(?:as\s+(?:a\s+)?draft\b|open\s+as\s+draft)`),
		content: "User wants PRs opened as drafts.",
		kind:    "preference",
	},

	// --- Proceed vs ask ---
	{
		pattern: regexp.MustCompile(`(?i)(just\s+do\s+it\b|proceed\s+(on\s+your\s+best\s+judgment)|go\s+ahead\b|don't\s+ask\s+(for\s+(clarification|permission)|me|before)|act\s+on\s+best\s+judgment|use\s+your\s+discretion\b|take\s+care\s+(of\s+it|this)\s+without\s+asking)`),
		content: "User prefers the assistant to proceed without asking for clarification — act on best judgment.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)(always\s+ask|before\s+doing anything|confirm\s+with me|get approval|check with me first)`),
		content: "User wants the assistant to confirm before acting — always ask for approval.",
		kind:    "preference",
	},

	// --- Review style ---
	{
		pattern: regexp.MustCompile(`(?i)(inline (comment|review)|nitpick style|line-by-line comments)`),
		content: "User prefers inline review comments instead of file-level summaries.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)(high level review|overview only|summary (comment|review)|no nits)`),
		content: "User wants high-level review summaries instead of inline comments.",
		kind:    "preference",
	},

	// --- Language / framework preference ---
	{
		pattern: regexp.MustCompile(`(?i)(prefer\s+|type the code in )((?:typescript|go|python|rust))\b`),
		content: "", // filled dynamically from match group below
		kind:    "preference",
	},

	// --- Communication style---
	{
		pattern: regexp.MustCompile(`(?i)(keep it friendly|be polite|use a warm tone|no sarcasm|don't be snarky)`),
		content: "User prefers friendly, polite communication without sarcasm.",
		kind:    "preference",
	},
	{
		pattern: regexp.MustCompile(`(?i)(keep it professional|professional tone only|formal (?:style|manner)|no jokes)`),
		content: "User prefers a professional, formal tone without jokes.",
		kind:    "preference",
	},

	// --- Iteration style ---
	{
		pattern: regexp.MustCompile(`(?i)(iterative feedback|show progress as you go|incremental delivery|step by step)`),
		content: "User prefers iterative, incremental progress — show work step by step rather than all at once.",
		kind:    "preference",
	},

	// --- Goal statements ---
	{
		pattern: regexp.MustCompile(`(?i)((?:my goal is|I want to|trying to get better at)\s+to\s+(.+))`),
		content: "", // filled dynamically — see MinePreferences below
		kind:    "goal",
	},

	// --- Limit statements ---
	{
		pattern: regexp.MustCompile(`(?i)(don't\s+(?:ever|need to)\s+test\b|no automated tests? needed|skip the tests?)\b`),
		content: "User does not need automated tests for their work.",
		kind:    "limit",
	},
}

// MinePreferences scans message text for explicitly stated user preferences and
// returns Candidate values ready to commit. Each match is deduplicated against
// already-known content (the caller's existing memories in the user bucket), so
// identical phrasings across turns are silently ignored.
//
// Pattern matching rules:
//   - Case-insensitive match on user utterance text.
//   - Excluded by a negative filter (if configured).
//   - Content is normalised to third-person present-tense for dedup stability.
//   - Only durable preferences are extracted — transient request details are
//     ignored ("for this PR" suffixes are stripped before matching).
func MinePreferences(message string) []memory.Candidate {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}

	const maxMatchRunes = 600 // cap: one @mention is short; strip after that to avoid long-thread tail noise
	if r := []rune(message); len(r) > maxMatchRunes {
		message = string(r[:maxMatchRunes])
	}

	// Strip transient per-request context that drowns out the preference.
	// Examples: "[on my-project#42]", "(PR-123)", "re: issue 7".
	var stripped strings.Builder
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		stripped.WriteString(trimmed)
		stripped.WriteByte(' ')
	}
	message = stripped.String()

	seen := make(map[string]bool)
	var out []memory.Candidate

	for _, rule := range preferenceRules {
		if !rule.pattern.MatchString(message) {
			continue
		}
		if rule.exclude != nil && rule.exclude.MatchString(message) {
			continue
		}

		content := rule.content
		if content == "" {
			if submatch := rule.pattern.FindStringSubmatch(message); len(submatch) > 0 {
				lang := strings.ToLower(submatch[len(submatch)-1])
				switch lang {
				case "typescript", "go", "python", "rust":
					content = fmt.Sprintf("User prefers code in %s.", normalizeLanguage(lang))
				}
			}
			if content == "" {
				// goal patterns extract the actual goal from matched text
				content = dynamicGoalFromMatch(message, rule.pattern)
			}
		}
		if strings.TrimSpace(content) == "" {
			continue // nothing to commit
		}

		if seen[content] {
			continue // dedup within same match set
		}
		seen[content] = true
		out = append(out, memory.Candidate{
			Content:  content,
			Metadata: map[string]string{"kind": rule.kind},
		})
	}

	return out
}

// normalizeLanguage converts shorthand language names to sentence-friendly form.
func normalizeLanguage(lang string) string {
	switch lang {
	case "go":
		return "Go"
	case "typescript":
		return "TypeScript"
	default:
		return strings.ToUpper(lang[:1]) + lang[1:] // Python, Rust, etc
	}
}

// dynamicGoalFromMatch extracts a goal statement from the matched text.
func dynamicGoalFromMatch(message string, pat *regexp.Regexp) string {
	match := pat.FindStringSubmatch(message)
	if len(match) < 3 {
		return ""
	}
	intent := strings.TrimSpace(strings.ToLower(match[1]))
	goalText := strings.TrimSpace(strings.ToLower(match[2]))

	switch {
	case strings.Contains(intent, "goal") || strings.Contains(intent, "want"):
		text := strings.TrimPrefix(goalText, "learn ")
		if text == "" {
			return "User wants to learn programming."
		}
		return fmt.Sprintf("User wants to learn %s.", extractTopic(text))
	case strings.Contains(intent, "trying"):
		text := strings.TrimPrefix(strings.TrimPrefix(goalText, "improve "), "master ")
		if text == "" {
			return "User is trying to improve their programming skills."
		}
		return fmt.Sprintf("User is trying to improve their skills in %s.", extractTopic(text))
	default:
		return ""
	}
}

// extractTopic pulls the first two meaningful words from topic text, stripping
// trailing conjunctions and punctuation.
func extractTopic(s string) string {
	s = strings.TrimRightFunc(s, func(r rune) bool { return r == ',' || r == '.' || r == '!' || r == '?' })
	endsWithAnd := strings.HasSuffix(s, " and")
	if endsWithAnd {
		s = strings.TrimSuffix(s, " and")
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return "programming"
	}
	end := len(words)
	if end > 2 {
		end = 2
	}
	result := strings.Join(words[:end], " ")
	if result == "" {
		result = "programming"
	}
	return result
}

// commitPreferences writes extracted preference candidates into the user
// bucket, keyed by userID. It runs through the normal Commit path (dedup +
// consolidate) so updated phrasing updates rather than duplicates existing
// memories. If store is nil or consolidation fails silently: this is best-effort,
// never blocks a run. Returns the number of candidates committed.
func commitPreferences(ctx context.Context, store *memory.Store, userID string, cands []memory.Candidate) int {
	if store == nil || len(cands) == 0 {
		return 0
	}

	sc := memory.Scope{User: userID, Legacy: userID}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	n, err := store.Commit(cctx, sc, "orchestrator", cands, "")
	if err != nil {
		// Best-effort: a failed preference commit logs but never blocks the run.
		// The next turn will retry; if the same preference is stated again it
		// reaches the consolidator once more.
		slog.Warn("preference commit failed", "component", "orchestrator", "user", userID, "err", err)
		return 0
	}
	if n > 0 {
		slog.Info("preferences committed", "component", "orchestrator", "user", userID, "count", n)
	}
	return n
}
