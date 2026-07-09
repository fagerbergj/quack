package tools

import (
	"fmt"
	"log/slog"
	"strings"

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
const askAdvisorDescription = "Consult your research advisor: a mentor who already knows this task's goal " +
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

// NewAskAdvisorTool returns ask_advisor: a worker consults its research
// advisor at its own discretion, as many times as it likes. Construction
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
// the model ever runs, so the read is deterministic and race-free (no
// persisted-event scan — the live 2026-07-09 failure). The token keys a
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

// consultAdvisor runs one advisor turn in its own isolated runner (mirrors how
// the judge runs isolated — internal/vetting/judge.go) over a session PERSISTED
// in the shared store (unlike the judge's throwaway in-memory one — the whole
// point here is that the mentor remembers). tc is only the context.Context for
// the run (agent.Context embeds it); identity comes from token, and seed is
// prepended to the first prompt of a brand-new thread.
func consultAdvisor(tc adkagent.Context, advisor adkagent.Agent, sessions session.Service, token, seed, request string) (string, error) {
	sessID := token + ":advisor"

	// A successful Get means the thread already exists — its history carries
	// the seed from the first consult, so don't repeat it. AutoCreateSession
	// on the runner below does the actual Create for a new thread.
	if _, err := sessions.Get(tc, &session.GetRequest{AppName: advisorAppName, UserID: advisorUserID, SessionID: sessID}); err == nil {
		seed = ""
	}

	r, err := runner.New(runner.Config{
		AppName: advisorAppName, Agent: advisor, SessionService: sessions, AutoCreateSession: true,
	})
	if err != nil {
		return "", fmt.Errorf("tools: advisor runner: %w", err)
	}

	prompt := request
	if seed != "" {
		prompt = seed + "\n\n---\n\nThe worker's request:\n" + request
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

	var out strings.Builder
	for ev, rerr := range r.Run(tc, advisorUserID, sessID, content, adkagent.RunConfig{}) {
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
