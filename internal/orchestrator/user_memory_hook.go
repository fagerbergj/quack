package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/stream"
)

// userMemoryPreFilter is a cheap, deterministic gate on whether a message
// MIGHT state a durable preference - broad on purpose, since a false positive
// only costs one wasted model call but a false negative costs a missed memory.
// This is the only acceptable use of regex here: a pre-filter that decides
// whether to WAKE the memory agent, never a decider of what to commit (that
// was #542's mistake - hardcoded regex rules that only catch known phrasings).
var userMemoryPreFilter = regexp.MustCompile(`(?i)\b(prefer|always|never|from now on|going forward|in the future|by default|instead of|rather than|don't|do not|no more|stop |i like|i want|i need|i hate|i love|keep it|make it|remember that|for me|my style|as a rule|as a habit)\b`)

// memoryAgentAppName/SessionID key the throwaway in-memory session each call
// runs in - fresh per call (see runMemoryAgent), so a constant name is fine.
const (
	memoryAgentAppName   = "quack-memory-agent"
	memoryAgentUserID    = "memory-agent"
	memoryAgentSessionID = "extract"
)

// maybeMineUserMemory fires the end-of-turn user-memory hook (#262). Gated on
// user memory being configured (o.userMem) AND the hook agent being wired
// (o.memAgent, config orchestrator.user_memory_hook.enabled) AND the cheap
// pre-filter matching - only then does it wake the memory agent. Entirely
// fire-and-forget: runs in its own goroutine on a context detached from the
// request, so it can never block, delay, or alter the user-facing response.
// Any failure (agent call, parse, commit) is a single logged WARN - never an
// error the caller sees.
func (o *Orchestrator) maybeMineUserMemory(ctx context.Context, userID, message string) {
	if o.userMem == nil || o.memAgent == nil {
		return
	}
	if !userMemoryPreFilter.MatchString(message) {
		return
	}
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		cands, err := mineUserMemory(bgCtx, o.memAgent, message)
		if err != nil {
			slog.Warn("user memory hook: extraction failed", "component", "orchestrator", "user", userID, "err", err)
			return
		}
		commitUserMemory(bgCtx, o.userMem, userID, cands)
	}()
}

// memoryCandidate is the memory agent's per-fact output shape (agents/memory-agent/prompt.md).
type memoryCandidate struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// mineUserMemory runs the memory agent once, fresh (a throwaway in-memory
// session - mirrors vetting.runWriterFresh), on message and parses its JSON
// array reply into commit-ready candidates. Candidates with empty content are
// dropped defensively; a model that emits prose around the JSON is tolerated
// via stripToJSONArray.
func mineUserMemory(ctx context.Context, memAgent adkagent.Agent, message string) ([]memory.Candidate, error) {
	r, err := runner.New(runner.Config{
		AppName: memoryAgentAppName, Agent: memAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return nil, err
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
	var out strings.Builder
	for ev, rerr := range r.Run(ctx, memoryAgentUserID, memoryAgentSessionID, content, adkagent.RunConfig{}) {
		if rerr != nil {
			return nil, rerr
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	raw := stream.StripThinking(out.String())
	var parsed []memoryCandidate
	if err := json.Unmarshal([]byte(stripToJSONArray(raw)), &parsed); err != nil {
		return nil, err
	}
	cands := make([]memory.Candidate, 0, len(parsed))
	for _, p := range parsed {
		if strings.TrimSpace(p.Content) == "" {
			continue
		}
		cands = append(cands, memory.Candidate{
			Content:  strings.TrimSpace(p.Content),
			Metadata: map[string]string{"kind": p.Kind},
		})
	}
	return cands, nil
}

// stripToJSONArray trims a model reply down to its outermost `[...]` — tolerates
// a stray markdown code fence or a leading/trailing sentence around the JSON
// the prompt asked for verbatim.
func stripToJSONArray(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < start {
		return "[]" // no array found ⇒ treat as "nothing to commit"
	}
	return s[start : end+1]
}

// commitUserMemory writes cands into userID's user bucket via the store's
// normal Commit path (dedup + consolidate), scoped exactly like #542's
// commitPreferences. Best-effort: a failure logs and is otherwise silent.
func commitUserMemory(ctx context.Context, store *memory.Store, userID string, cands []memory.Candidate) {
	if store == nil || len(cands) == 0 {
		return
	}
	sc := memory.Scope{User: userID, Legacy: userID}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := store.Commit(cctx, sc, orchestratorName, cands, "")
	if err != nil {
		slog.Warn("user memory hook: commit failed", "component", "orchestrator", "user", userID, "err", err)
		return
	}
	if n > 0 {
		slog.Info("user memory hook: committed", "component", "orchestrator", "user", userID, "count", n)
	}
}
