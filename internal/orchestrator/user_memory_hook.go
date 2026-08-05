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

// userMemoryPreFilter: cheap gate on whether a message might state a preference; false positives are cheap.
var userMemoryPreFilter = regexp.MustCompile(`(?i)\b(prefer|always|never|from now on|going forward|in the future|by default|instead of|rather than|don't|do not|no more|stop |i like|i want|i need|i hate|i love|keep it|make it|remember that|for me|my style|as a rule|as a habit)\b`)

// memoryAgentAppName/etc: throwaway in-memory session per call (fresh per call, constant name fine).
const (
	memoryAgentAppName   = "quack-memory-agent"
	memoryAgentUserID    = "memory-agent"
	memoryAgentSessionID = "extract"
)

// maybeMineUserMemory: fire-and-forget end-of-turn user-memory hook; never blocks the response.
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

// mineUserMemory: run memory agent once, parse JSON array reply into commit-ready candidates.
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

// stripToJSONArray trims model reply to outermost `[...]`, tolerating stray text around JSON.
func stripToJSONArray(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < start {
		return "[]" // no array found ⇒ treat as "nothing to commit"
	}
	return s[start : end+1]
}

// commitUserMemory writes candidates via the store's Commit path; best-effort, failure is logged silently.
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
