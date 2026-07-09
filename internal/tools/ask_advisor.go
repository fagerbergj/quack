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

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// advisorAppName namespaces the advisor's own persistent sessions in the
// shared session.Service, distinct from the "quack" app the chat/plan
// sessions use — so an advisor SessionID can never collide with a chat's.
const advisorAppName = "quack-advisor"

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
// The handler derives the calling node's identity from the tool context
// (session id + NodeInfo path — same discipline the gate uses in
// internal/vetting/node.go's pathHasNode; see nodeIDFromSession for why it
// re-fetches the session rather than reading agent.Context.Path()/Session()
// directly) and gets-or-creates a persistent PER-NODE advisor session keyed
// by invocation + node, so the mentor's memory of this task's conversation
// survives across gate rounds (draft → revision) and HITL pause/resume (both
// keep the same invocation ID) but never interleaves between concurrent nodes
// or across unrelated plan runs. On first creation the session opens with the
// node's task + acceptance rubric (seeded from session state written by
// dag.newGatedNode — see dag.NodeTaskStateKey/NodeRubricStateKey), so the
// mentor knows the desired outcome from its very first reply.
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
			nodeID := nodeIDFromSession(tc, sessions)
			if nodeID == "" {
				slog.Warn("ask_advisor: could not derive node identity from the session; proceeding without advice",
					"component", "tools")
				return askAdvisorResult{}, nil
			}
			advice, err := consultAdvisor(tc, advisor, sessions, nodeID, args.Request)
			if err != nil {
				slog.Warn("ask_advisor: consult failed; proceeding without advice",
					"component", "tools", "node", nodeID, "err", err)
				return askAdvisorResult{}, nil
			}
			return askAdvisorResult{Advice: advice}, nil
		},
	)
}

// nodeIDFromSession derives the calling node's ID from the tool context.
//
// A tool context deliberately restricts what a tool can see of the running
// graph: agent.Context.Path()/RunID() are empty inside a tool call (an
// AgentNode-wrapped LlmAgent gets a FRESH InvocationContext for its own run —
// see workflow/agent_node.go — that carries Session/Branch/InvocationID but
// NOT the dynamic-node Path/RunID a live ctx.Path() would need), and
// Session() is explicitly blocked (returns nil, logged "not supported").
// SessionID()/UserID()/AppName()/FunctionCallID(), however, all resolve
// correctly. So: re-fetch the session by those identifiers (same store, a
// fresh session.Service.Get — sidesteps the Session() block entirely) and
// find the FunctionCall event matching tc.FunctionCallID() — that is THIS
// exact ask_advisor call, and the scheduler stamps every event a node's
// worker emits with NodeInfo.Path (dynamicSubScheduler wraps the child's
// yielded events), so that event's path names the calling node. Mirrors
// vetting/node.go's pathHasNode/scanNodeAsks discipline (scan events, match
// by a stable ID, read NodeInfo) — just via an independently-fetched session
// instead of ctx.Session(), which a tool can't call directly.
func nodeIDFromSession(tc adkagent.Context, sessions session.Service) string {
	resp, err := sessions.Get(tc, &session.GetRequest{AppName: tc.AppName(), UserID: tc.UserID(), SessionID: tc.SessionID()})
	if err != nil || resp == nil || resp.Session == nil {
		return ""
	}
	fcID := tc.FunctionCallID()
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil || ev.NodeInfo == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.ID == fcID {
				return nodeIDFromPath(ev.NodeInfo.Path)
			}
		}
	}
	return ""
}

// consultAdvisor runs one advisor turn in its own isolated runner (mirrors how
// the judge runs isolated — internal/vetting/judge.go) over a session PERSISTED
// in the shared store (unlike the judge's throwaway in-memory one — the whole
// point here is that the mentor remembers). tc doubles as the context.Context
// for the run and as the source of the node's seeded task/rubric on first
// creation (agent.Context embeds context.Context).
func consultAdvisor(tc adkagent.Context, advisor adkagent.Agent, sessions session.Service, nodeID, request string) (string, error) {
	userID := tc.UserID()
	sessID := tc.InvocationID() + ":" + nodeID + ":advisor"

	// A Get failure (not found, or any other read error) means this is the
	// first consult for this node this invocation — seed it. AutoCreateSession
	// on the runner below does the actual Create; we only need to know here
	// whether to prepend the seed text to the first prompt.
	seed := ""
	if _, err := sessions.Get(tc, &session.GetRequest{AppName: advisorAppName, UserID: userID, SessionID: sessID}); err != nil {
		seed = seedText(tc, nodeID)
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
	for ev, rerr := range r.Run(tc, userID, sessID, content, adkagent.RunConfig{}) {
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
// session: the node's task + acceptance rubric, read from session state
// (written by dag.newGatedNode before the worker runs — the tool is built
// once per agent bundle at startup and shared across every node, so Task/
// Rubric can't be closed over at construction). Empty when neither is set
// (e.g. a test harness that never seeded state) — the advisor still runs, it
// just doesn't know the desired outcome up front.
func seedText(tc adkagent.Context, nodeID string) string {
	st := tc.State()
	if st == nil {
		return ""
	}
	task, _ := st.Get(dag.NodeTaskStateKey + nodeID)
	rubric, _ := st.Get(dag.NodeRubricStateKey + nodeID)
	taskStr, _ := task.(string)
	rubricStr, _ := rubric.(string)
	if taskStr == "" && rubricStr == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You are advising on the following task for the rest of this session.\n\nTask:\n")
	sb.WriteString(taskStr)
	if rubricStr != "" {
		sb.WriteString("\n\nAcceptance rubric (what a passing answer must satisfy):\n")
		sb.WriteString(rubricStr)
	}
	return sb.String()
}

// nodeIDFromPath extracts the plan node's ID from an event's NodeInfo path —
// e.g. "quack-plan-graph@1/n1@1/web-researcher@worker-r0" (or, without a
// top-level wrapper, "n1/web-researcher@worker-r0") — the immediate parent
// segment of the worker's own run segment, stripped of its "@run" suffix.
// Works regardless of how deep the gated node itself is nested. Mirrors the
// segName discipline in dag/executor.go and vetting/node.go's pathHasNode.
// Empty when the path is too shallow to have a parent (the worker run
// standalone, outside any gated node).
func nodeIDFromPath(path string) string {
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return ""
	}
	seg := segs[len(segs)-2]
	if i := strings.IndexByte(seg, '@'); i >= 0 {
		seg = seg[:i]
	}
	return seg
}
