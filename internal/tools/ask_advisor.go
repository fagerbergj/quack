package tools

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// advisorAppName: separate namespace so advisor sessions never collide with chat sessions.
const advisorAppName = "quack-advisor"

// advisorUserID: fixed user for all advisor sessions, sidestepping A2A user-ID discontinuity.
const advisorUserID = "advisor"

// askAdvisorDescription: the relationship contract.
const askAdvisorDescription = "Consult your advisor: a mentor who already knows this task's goal " +
	"and its acceptance rubric, and is always available to help you reach it - without doing the work for " +
	"you. The relationship works like a good academic advising relationship: honest and direct feedback " +
	"(it will tell you plainly when an approach is off track, too broad, too narrow, or missing something - " +
	"not soften a real problem into vague encouragement), and a real memory of everything you've discussed " +
	"together on this task - you never need to re-explain earlier context or re-ask a settled question.\n\n" +
	"Consult at these moments:\n" +
	"- BEFORE committing to an approach - sanity-check your plan while it's still cheap to change.\n" +
	"- When the task's scope or intent is ambiguous and a second opinion would resolve it.\n" +
	"- When you're stuck, or your searches/fetches aren't converging on an answer.\n" +
	"- After you receive critical feedback (e.g. a failed review) - consult before revising, so your next " +
	"attempt actually fixes the gap instead of guessing again.\n\n" +
	"A good request is SPECIFIC: say what you're about to do (or where you're stuck) and why, not just " +
	"\"is this good?\" - e.g. \"I'm planning to answer this by comparing three vendors' pricing pages; am I " +
	"missing an angle?\" beats \"help\".\n\n" +
	"The advisor will not write your answer, run searches for you, or make the call for you - expect " +
	"guidance and pointed questions back, not a finished draft. Use its advice to inform your own work; it " +
	"is not a substitute for doing the task yourself."

type askAdvisorArgs struct {
	Request string `json:"request"`
}

type askAdvisorResult struct {
	Advice string `json:"advice"`
}

// NewAskAdvisorTool: worker consults its advisor. Node identity derived from the advisor-thread marker.
func NewAskAdvisorTool(advisor adkagent.Agent, sessions session.Service) (tool.Tool, error) {
	return functiontool.New[askAdvisorArgs, askAdvisorResult](
		functiontool.Config{
			Name:        "ask_advisor",
			Description: askAdvisorDescription,
		},
		func(tc adkagent.Context, args askAdvisorArgs) (askAdvisorResult, error) {
			if advisor == nil || sessions == nil {
				return askAdvisorResult{}, nil
			}
			token, seed := advisorThread(tc)
			advice, err := consultAdvisor(tc, advisor, sessions, token, seed, args.Request)
			if err != nil {
				slog.Warn("ask_advisor: consult failed; proceeding without advice",
					"component", "tools", "thread", token, "err", err)
				return askAdvisorResult{}, nil
			}
			return askAdvisorResult{Advice: advice}, nil
		},
	)
}

// advisorThread resolves the mentor conversation this call belongs to.
func advisorThread(tc adkagent.Context) (token, seed string) {
	if tok, ok := vetting.ParseAdvisorThread(contentText(tc.UserContent())); ok {
		if task, found := vetting.LookupAdvisorThread(tok); found {
			seed = seedText(task)
		}
		return tok, seed
	}
	return tc.AppName() + "/" + tc.SessionID(), ""
}

// contentText concatenates a content's plain-text parts.
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && !p.Thought && p.Text != "" {
			sb.WriteString(p.Text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// advisorThreadLocks: serializes consults per thread token.
var advisorThreadLocks sync.Map

func advisorThreadLock(token string) *sync.Mutex {
	mu, _ := advisorThreadLocks.LoadOrStore(token, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// isSessionConflict: reports whether err is a transient optimistic-concurrency conflict.
func isSessionConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stale session error") ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") // Postgres' unique-violation wording
}

// consultAdvisor: runs one advisor turn, serialized per token with retry on optimistic-locking conflicts.
func consultAdvisor(ctx context.Context, advisor adkagent.Agent, sessions session.Service, token, seed, request string) (string, error) {
	mu := advisorThreadLock(token)
	mu.Lock()
	defer mu.Unlock()

	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			// Jittered backoff.
			select {
			case <-time.After(25*time.Millisecond + time.Duration(rand.Int64N(50))*time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			slog.Debug("ask_advisor: retrying after session conflict",
				"component", "tools", "thread", token, "attempt", attempt, "err", lastErr)
		}
		advice, err := consultOnce(ctx, advisor, sessions, token, seed, request)
		if err == nil {
			return advice, nil
		}
		if !isSessionConflict(err) {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

// consultOnce: single consult attempt - check/seed the thread, run the advisor in an isolated runner.
func consultOnce(ctx context.Context, advisor adkagent.Agent, sessions session.Service, token, seed, request string) (string, error) {
	sessID := token + ":advisor"

	// Thread already exists - don't reseed.
	seedThis := seed
	if _, err := sessions.Get(ctx, &session.GetRequest{AppName: advisorAppName, UserID: advisorUserID, SessionID: sessID}); err == nil {
		seedThis = ""
	}

	r, err := runner.New(runner.Config{
		AppName: advisorAppName, Agent: advisor, SessionService: sessions, AutoCreateSession: true,
	})
	if err != nil {
		return "", fmt.Errorf("tools: advisor runner: %w", err)
	}

	prompt := request
	if seedThis != "" {
		prompt = seedThis + "\n\n---\n\nThe worker's request:\n" + request
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

	var out strings.Builder
	for ev, rerr := range r.Run(ctx, advisorUserID, sessID, content, adkagent.RunConfig{}) {
		if rerr != nil {
			return "", rerr
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
	return stream.StripThinking(out.String()), nil
}

// seedText: builds mentor's opening context (task + rubric from the advisor-task registry).
func seedText(t vetting.AdvisorTask) string {
	if t.Task == "" && t.Rubric == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You are advising on the following task for the rest of this session.\n\nTask:\n")
	sb.WriteString(t.Task)
	if t.Rubric != "" {
		sb.WriteString("\n\nAcceptance rubric (what a passing answer must satisfy):\n")
		sb.WriteString(t.Rubric)
	}
	return sb.String()
}
