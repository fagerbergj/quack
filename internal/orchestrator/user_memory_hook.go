package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/stream"
)

// userMemoryPreFilter: cheap gate on whether a message might state a
// preference. Narrowed to preference-shaped phrases (#1283 audit finding 9):
// bare never/always/instead of/don't fired on 26.1% of a 2,389-paragraph technical-prose corpus (this repo's commit messages) - four keywords alone were 91% of those hits. This alternation measured 0.3% on the same corpus while still matching "always use tabs".
var userMemoryPreFilter = regexp.MustCompile(`(?i)\b(from now on|going forward|by default|as a rule|remember that|for me|i (prefer|like|want|need|hate|love)|(please )?(always|never) (use|do|write|make|include|add|call|run|reply|respond|ask|prefer))\b`)

// memoryAgentAppName/etc: throwaway in-memory session per call, isolated by a
// fresh InMemoryService, not by session id. memoryAgentSessionID is only the
// fallback when no chat id is available (see mineUserMemory).
const (
	memoryAgentAppName   = "quack-memory-agent"
	memoryAgentUserID    = "memory-agent"
	memoryAgentSessionID = "extract"
)

// memoryExtract observes the memory-agent extraction against the user's own words (never the
// assistant's reply), including turns the keyword pre-filter skips.
var memoryExtract = decide.RegisterObserveNoul("memory.extract", "durable_fact", "Does this turn state a durable fact about the user "+
	"(a preference, identity, standing instruction, or ongoing project) worth remembering beyond this conversation?", "memory_extract_call")

// memoryExtractMessageMax caps each message in memory.extract's state: ~6 KB in all, under Clef's 4096-token cap.
const memoryExtractMessageMax = 3000

type memoryExtractState struct {
	Message  string `json:"message"`
	Previous string `json:"previous_message,omitempty"`
}

// maybeMineUserMemory: fire-and-forget end-of-turn user-memory hook; never blocks the response.
// Call turnEnded once the turn's answer is persisted; it only spawns a goroutine.
func (o *Orchestrator) maybeMineUserMemory(ctx context.Context, userID, chatID, source, message string) (turnEnded func()) {
	if o.userMem == nil || o.memAgent == nil {
		return func() {}
	}
	prefiltered := !userMemoryPreFilter.MatchString(message)
	baseline := make(chan string, 1)
	turnEnded = o.observeMemoryExtract(ctx, userID, chatID, message, prefiltered, baseline)
	if prefiltered {
		baseline <- "false"
		return turnEnded
	}
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		cands, err := mineUserMemory(bgCtx, o.memAgent, message, chatID)
		if err != nil {
			baseline <- ""
			slog.Warn("user memory hook: extraction failed", "component", "orchestrator", "user", userID, "err", err)
			return
		}
		baseline <- strconv.FormatBool(len(cands) > 0)
		commitUserMemory(bgCtx, o.userMem, userID, memory.Provenance{ChatID: chatID, Source: source}, cands)
	}()
	return turnEnded
}

// observeMemoryExtract returns the turn-end step that asks memory.extract about message and the
// user's previous message, settling with baseline whenever extraction finishes.
func (o *Orchestrator) observeMemoryExtract(ctx context.Context, userID, chatID, message string, prefiltered bool, baseline <-chan string) func() {
	if !o.decisions.Enabled(memoryExtract.ID) {
		return func() {}
	}
	ctx = context.WithoutCancel(ctx)
	// Read before this turn's own message is persisted, so the last user turn is the previous one.
	previous := lastUserTurn(o.PriorEvents(ctx, userID, chatID))
	return func() {
		go func() {
			state := decide.Annotated{
				State: memoryExtractState{Message: decide.Clip(message, memoryExtractMessageMax), Previous: decide.Clip(previous, memoryExtractMessageMax)},
				Meta:  map[string]bool{"prefiltered": prefiltered},
			}
			o.decisions.Observe(ctx, memoryExtract.Point, state)(<-baseline)
		}()
	}
}

// lastUserTurn is the text of the last user turn in events, "" for none.
func lastUserTurn(events []*session.Event) string {
	turns := buildHistory(events)
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].Text
		}
	}
	return ""
}

// memoryCandidate is the memory agent's per-fact output shape (agents/memory-agent/prompt.md).
type memoryCandidate struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// mineUserMemory: run memory agent once, parse JSON array reply into commit-ready candidates.
func mineUserMemory(ctx context.Context, memAgent adkagent.Agent, message, chatID string) ([]memory.Candidate, error) {
	r, err := runner.New(runner.Config{
		AppName: memoryAgentAppName, Agent: memAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return nil, err
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: message}}}
	// Real chat id groups this run under its causing chat in Langfuse
	// (gen_ai.conversation.id); empty falls back rather than emitting "".
	sessionID := memoryAgentSessionID
	if chatID != "" {
		sessionID = chatID
	}
	var out strings.Builder
	for ev, rerr := range r.Run(ctx, memoryAgentUserID, sessionID, content, adkagent.RunConfig{}) {
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
func commitUserMemory(ctx context.Context, store *memory.Store, userID string, prov memory.Provenance, cands []memory.Candidate) {
	if store == nil || len(cands) == 0 {
		return
	}
	sc := memory.Scope{User: userID, Legacy: userID}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, err := store.Commit(cctx, sc, orchestratorName, prov, cands, "")
	if err != nil {
		slog.Warn("user memory hook: commit failed", "component", "orchestrator", "user", userID, "err", err)
		return
	}
	if n > 0 {
		slog.Info("user memory hook: committed", "component", "orchestrator", "user", userID, "count", n)
	}
}
