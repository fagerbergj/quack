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

// advisorAppName namespaces the advisor's own persistent sessions in the
// shared session.Service, distinct from the "quack" app the chat/plan
// sessions use — so an advisor SessionID can never collide with a chat's.
const advisorAppName = "quack-advisor"

// advisorUserID is the fixed user for all advisor sessions. The thread token
// in the session ID already uniquely identifies the conversation (plan +
// node), and a fixed user sidesteps the client/server user-ID discontinuity
// across the A2A hop: the tool executes in the A2A server's runner, whose
// user is NOT the chat's, and may differ between the dispatch that drafts
// and the one that revises.
const advisorUserID = "advisor"

// askAdvisorDescription is the relationship contract — the main lever for
// healthy consultation frequency. Written from the mentorship research in
// .quack/advisor-mentor-research.md (UC Berkeley/Harvard/NIH advising
// guidance): availability + honest, direct feedback + a real memory of the
// relationship (no re-briefing), when a good advisee consults, and what a
// good request looks like.
const askAdvisorDescription = "Consult your advisor: a mentor who already knows this task's goal " +
	"and its acceptance rubric, and is always available to help you reach it — without doing the work for " +
	"you. The relationship works like a good academic advising relationship: honest and direct feedback " +
	"(it will tell you plainly when an approach is off track, too broad, too narrow, or missing something — " +
	"not soften a real problem into vague encouragement), and a real memory of everything you've discussed " +
	"together on this task — you never need to re-explain earlier context or re-ask a settled question.\n\n" +
	"Consult at these moments:\n" +
	"- BEFORE committing to an approach — sanity-check your plan while it's still cheap to change.\n" +
	"- When the task's scope or intent is ambiguous and a second opinion would resolve it.\n" +
	"- When you're stuck, or your searches/fetches aren't converging on an answer.\n" +
	"- After you receive critical feedback (e.g. a failed review) — consult before revising, so your next " +
	"attempt actually fixes the gap instead of guessing again.\n\n" +
	"A good request is SPECIFIC: say what you're about to do (or where you're stuck) and why, not just " +
	"\"is this good?\" — e.g. \"I'm planning to answer this by comparing three vendors' pricing pages; am I " +
	"missing an angle?\" beats \"help\".\n\n" +
	"The advisor will not write your answer, run searches for you, or make the call for you — expect " +
	"guidance and pointed questions back, not a finished draft. Use its advice to inform your own work; it " +
	"is not a substitute for doing the task yourself."

type askAdvisorArgs struct {
	// Request is what you want advice on — what you're about to do (or where
	// you're stuck) and why. Be specific; see the tool description.
	Request string `json:"request"`
}

type askAdvisorResult struct {
	// Advice is the mentor's reply. Empty when the advisor is unavailable or
	// the consult failed — best-effort, so the worker is never blocked on it.
	Advice string `json:"advice"`
}

// NewAskAdvisorTool returns ask_advisor: a worker consults its advisor at
// its own discretion, as many times as it likes. Construction
// takes the advisor agent and the shared session.Service; a nil advisor is
// tolerated defensively (production never registers the tool in that case —
// see resolveToolNames in internal/serve) and always yields empty advice.
//
// The handler derives the calling node's identity from the advisor-thread
// marker the gate stamps into every worker prompt (dag.newGatedNode →
// vetting.AdvisorThreadMarker), read back out of tc.UserContent(). That is
// the ONLY channel that survives the production A2A hop: the tool executes
// inside the worker's A2A server runner (internal/agent.Serve), where the
// calling runner's session, state, NodeInfo, and branch are all invisible —
// but the prompt IS the inbound A2A message, and UserContent is fixed before
// the model ever runs, so the read is deterministic and race-free. The token keys a
// persistent PER-NODE advisor session (`<plan>/<node>:advisor`), so the
// mentor's memory survives gate rounds (draft → revision), steered re-runs,
// and HITL pause/resume — the gate re-derives the same token from plan+node
// every round — but never interleaves between concurrent nodes or across
// plans. On a thread's first consult the session opens with the node's task +
// acceptance rubric (from the registry dag.newGatedNode fills — see
// vetting/advisor_thread.go), so the mentor knows the desired outcome from
// its very first reply.
//
// A prompt WITHOUT a marker (the agent invoked directly, outside any gated
// node) falls back to a per-conversation thread keyed by the calling app +
// session — the mentor still works, it just isn't task-seeded.
//
// Errors (a broken session store, an advisor model failure) return empty
// advice with a logged warning — best-effort, never fails the calling worker.
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

// advisorThread resolves the mentor conversation this call belongs to: the
// thread token plus the seed text for a brand-new session. The gate's marker
// in the prompt (tc.UserContent()) names the node's thread and keys the
// registered task+rubric; without a marker (direct, un-gated invocation) the
// thread falls back to the calling conversation itself, unseeded.
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

// advisorThreadLocks serializes consults PER THREAD TOKEN. ADK executes a
// model turn's function calls in concurrent goroutines (llminternal/
// base_flow.go handleFunctionCalls), so one worker turn can fire two
// ask_advisor calls at once — two runner lifecycles (Get/Create → append)
// each holding its own localSession snapshot of the SAME advisor session row.
// Under the database service's optimistic locking the loser's append dies
// with "stale session error", the create race dies
// with a UNIQUE violation, and a double Get-miss double-seeds the thread.
// Serializing per token removes all three at the source and is the right
// conversation shape anyway (two simultaneous questions to one mentor are
// sequential turns); consults on DIFFERENT threads (concurrent nodes) don't
// contend. Entries are tiny and reusable; they are not deleted.
var advisorThreadLocks sync.Map

func advisorThreadLock(token string) *sync.Mutex {
	mu, _ := advisorThreadLocks.LoadOrStore(token, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// isSessionConflict reports whether err is a transient optimistic-concurrency
// conflict on the advisor session row: the database service's stale-session
// check (session/database/service.go applyEvent) or the create race's UNIQUE
// violation. These are retry-by-design; anything else is a real failure.
func isSessionConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stale session error") ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") // Postgres' unique-violation wording
}

// consultAdvisor runs one advisor turn for the given thread, serialized per
// token and with a small bounded retry on optimistic-locking conflicts
// (re-fetch + re-run; the seed check re-runs each attempt so a retried first
// consult can't double-seed). The per-token lock prevents same-process
// conflicts outright; the retry covers what it can't (e.g. Postgres rounds
// timestamp(6) half-up on write while the snapshot's UnixMicro() truncates,
// so a freshly created row can read ~1µs newer than its own snapshot).
func consultAdvisor(ctx context.Context, advisor adkagent.Agent, sessions session.Service, token, seed, request string) (string, error) {
	mu := advisorThreadLock(token)
	mu.Lock()
	defer mu.Unlock()

	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			// Small jittered backoff before re-fetching (25–75ms).
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

// consultOnce is a single consult attempt: check/seed the thread, run the
// advisor in its own isolated runner (mirrors how the judge runs isolated —
// internal/vetting/judge.go) over a session PERSISTED in the shared store
// (unlike the judge's throwaway in-memory one — the whole point here is that
// the mentor remembers). Identity comes from token; seed is prepended to the
// first prompt of a brand-new thread.
func consultOnce(ctx context.Context, advisor adkagent.Agent, sessions session.Service, token, seed, request string) (string, error) {
	sessID := token + ":advisor"

	// A successful Get means the thread already exists — its history carries
	// the seed from the first consult, so don't repeat it. AutoCreateSession
	// on the runner below does the actual Create for a new thread.
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

// seedText builds the mentor's opening context for a brand-new advisor
// thread: the node's task + acceptance rubric (registered by dag.newGatedNode
// before the worker runs — the tool is built once per agent bundle at startup
// and shared across every node, so Task/Rubric can't be closed over at
// construction; the registry is how the per-run value reaches it). Empty when
// neither field is set — the advisor still runs, it just doesn't know the
// desired outcome up front.
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
