package vetting

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

// PlanJudge: decides whether a proposed DAG plan is a well-formed answer to
// the user's request. repoKey is the plan's normalized repo identity (e.g.
// "github.com/owner/repo") when the plan declares setup, "" otherwise.
type PlanJudge func(ctx context.Context, request, planSummary, repoKey string) (accept bool, reason string, err error)

// submitPlanVerdictTool: structured-termination tool for plan judge, mirrors submit_verdict.
const submitPlanVerdictTool = "submit_plan_verdict"

type planVerdictArgs struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason"`
}

// planRubricInstruction: five-criterion rubric for plan shape (not answer quality), replaces old regex routing.
const planRubricInstruction = `You are an independent reviewer judging one STEP of a PROPOSED PLAN - a DAG of agent nodes, some already run - against the user's actual request and the conversation so far. You did not author the plan, you have no tools, and you are not judging any node's output quality; you are judging whether running this step is the right move.

You will be shown the user's request, the proposed plan (each node's id, agent, task, and dependencies - a node marked "ALREADY RAN" also shows its result), and the plan's declared setup (working clone + branch) and delivery (how the result reaches GitHub) - or "(none declared - this step may be a partial plan, more nodes to follow)" for either.

A plan is not required to be complete. It is built incrementally: the orchestrator plans what it knows, runs it, reads what came back, then hires or re-briefs whoever it needs for the rest - repeating until the request is satisfied, at which point it declares delivery. A plan with no delivery declared is a STATEMENT that more work follows, not a defect - judge it as one step, not as a finished answer.

A plan step is ACCEPTABLE only when ALL five hold:
1. Makes progress toward what was asked, given the whole conversation and what already ran. Read the request, then ask: does it hand back what the request asked to receive, given what this step runs on top of whatever already-run nodes produced? Delivery is the terminal marker: when plan.Delivery IS declared, apply the stricter check - the TERMINAL node's own output IS what the user receives, verbatim. Nothing runs after the graph: no synthesis step, no final response that rewrites, summarises, or turns findings into the deliverable. Name the artifact the request wants (a plan, a review, shipped code, an answer, a document), find the node whose own task actually produces it, and confirm that node is the plan's terminal (last, undepended-on) node - not merely a step feeding one. A terminal node that only explores, investigates, or reports never satisfies a request whose deliverable is a plan, a review, or shipped code, however thorough its report is - its report IS the answer, not raw material for one. The live failure this guards against: a single code-explorer node tasked to "produce a detailed report" was twice accepted, terminally, for a request that asked for an implementation plan, on the reasoning that the findings "will be used to write the plan in the final response" - there is no such step, so the exploration report was posted as the plan; that failure is about a plan that DECLARED itself done while only exploring, not about an explorer node that is honestly one step of several. The reverse also fails: a plan that declares delivery while stopping at exploring or reviewing when the request asked for shipped code. When plan.Delivery is NOT declared, only require that this step is a genuine, non-redundant move toward that eventual terminal node - a node that idles, repeats an already-run node's work, or wanders off into work the request never asked for fails this criterion even with no delivery declared yet.
2. Grounded - the plan is specific to this request (names real files/conventions where known, or has a node establish them), not generic boilerplate that could apply to any repo.
3. Minimal, correct decomposition - no obvious over- or under-splitting, and is sized to the SCOPE the request itself sets: when the request names specific commits, threads, files, or questions ("verify commit X resolves the finding", "a scoped re-check of those three threads - NOT a full re-review", "just check the auth changes"), the plan must match THAT scope, not the whole PR/repo - inflating a narrow ask into a full review or re-implementation fails this criterion exactly like ignoring part of a broad ask does, and this scope check is never a license to expand past what was asked just because the surrounding PR or repo is large.
4. Verifiable - where the plan changes code, some node's task names how the change will be checked in concrete terms: specific test file(s) or test target(s) to add/run, and the actual runnable command(s) (or, when the exact command can't be known before cloning, an instruction to derive and run the repo's real build/lint/test commands) - not a vague "add tests" or "verify it works" with no named target.
5. Independent, goal-scoped decomposition - decompose by independent PORTION of work with a clear, articulable goal, NEVER by activity or layer. A single cohesive change - one goal, stated in one sentence - is CORRECTLY a SINGLE code-implementer node, even though that node's own job includes implementing the API and the logic, writing its own tests, and running checks: splitting "API implementation" vs. "logic implementation" vs. "testing/verification" vs. "run checks" vs. "commit" into separate nodes for what is really one goal FAILS this criterion just as surely as a monolithic node covering two unrelated goals does. Require a CHAIN of code-implementer nodes wired by depends_on only when the request genuinely contains multiple INDEPENDENT portions - distinct goals that could be pursued, reviewed, and tested separately - with node N building on node N-1's committed work through the shared clone. A portion whose goal can't be stated in one sentence is really two portions and belongs in its own node; treat unusual size or complexity as a SIGNAL of that, not the axis itself - a portion so large or tangled it can't be reviewed and tested as one coherent unit is probably more than one goal in disguise, under-decomposed, and should fail this criterion. A node may make as many atomic commits as its own portion naturally needs - that granularity, like its tests, belongs to the implementer, not the plan. The same activity-vs-portion trap applies to a REVIEW plan: a code-explorer node whose task is to gather findings for a downstream code-reviewer to validate and post is "explore" and "review" split by ACTIVITY, not by independent portion - the two nodes review the same diff twice under two different rubrics for one job. A single-PR review of a small or cohesive diff (roughly under ~15 files or ~800 changed lines, one or a few related packages) is CORRECTLY one code-reviewer node; an explorer feeding a reviewer for a change that size FAILS this criterion exactly like the monolithic-vs-split-by-activity implementer case above. Fanning out a genuinely large, multi-subsystem review is only correct as MULTIPLE code-reviewer nodes (one per independent subsystem slice, each posting its own findings for the run to merge) - never an explorer pre-pass.

Read what the request actually asks to RECEIVE before judging criterion 1 - the most common mistake is inferring the deliverable from a verb the request happens to contain rather than what it explicitly asks for (e.g. an issue whose body describes what the EVENTUAL implementation PR should look like, while THIS request only asks for the plan). Judge what THIS request asks the plan to hand back, not every verb it contains.

Give the plan the benefit of the doubt on ambiguous phrasing - this check exists to catch a plan that is CLEARLY the wrong shape for the request (a terminal exploration/report node standing in for a requested plan, review, or implementation, or vice versa; a monolithic implementer where the feature obviously decomposes into several independent portions; a single cohesive change pointlessly split into activity-scoped nodes; an explicitly narrowed ask forced into the large-diff fan-out pattern it never asked for; or a step that adds nothing - idle, redundant with an already-ran node, or off-request), not to nitpick a reasonable decomposition or penalize a plan for being honestly unfinished.

Call submit_plan_verdict exactly once with accept (bool) and reason (if rejecting: name the ONE specific edit that fixes it - e.g. "add a terminal node that actually writes the plan - the current terminal node only explores", "this is a plan-only request; drop the code-implementer node", "declare delivery so the shipped code can reach GitHub", "split the implementer into a chain of independent, goal-scoped nodes", or "shrink this to the commit/threads/files the user actually named instead of the whole PR" - never a vague "reconsider the plan"; if accepting: a brief reason is fine, or "").`

// NewPlanJudge: builds PlanJudge backed by judgeModel (reuses the trust gate's judge). Isolated in-memory
// session per call. maxOutputTokens caps the round's own reply tokens; <= 0 leaves it uncapped (#889).
// thinkingLevel is gates.judge.thinking_level ("", "low", "medium", "high"); "" sends no ThinkingConfig. mem/led are optional (epic #1255 P2): nil skips the project-memory section entirely.
func NewPlanJudge(judgeModel model.LLM, maxOutputTokens int, thinkingLevel string, mem *memory.Store, led ledger.LedgerStore) PlanJudge {
	return func(ctx context.Context, request, planSummary, repoKey string) (bool, string, error) {
		var memSection string
		if mem != nil {
			coords := ledger.CoordsFromContext(ctx)
			// ponytail: repo scope only when the plan declares setup (repoKey
			// non-empty); user-only ceiling remains for chats with no origin.
			hits, _ := mem.RecallForTool(ctx, memory.Scope{User: coords.User, Repo: repoKey}, request, 0)
			if len(hits) > 0 {
				memSection = planMemorySection(hits)
				mem.LogRecall(ctx, led, coords.ChatID, "", "plan_judge", hits)
			}
		}
		var sink planVerdictArgs
		var submitted bool
		submit, err := functiontool.New[planVerdictArgs, map[string]any](
			functiontool.Config{
				Name:        submitPlanVerdictTool,
				Description: "Record your final accept/reject verdict on the plan and end the evaluation. Call this exactly once.",
			},
			func(tc adkagent.Context, a planVerdictArgs) (map[string]any, error) {
				sink = a
				submitted = true
				tc.Actions().Escalate = true
				tc.Actions().SkipSummarization = true
				return map[string]any{"recorded": true}, nil
			},
		)
		if err != nil {
			return false, "", fmt.Errorf("vetting: build plan judge tool: %w", err)
		}
		judgeAgent, err := llmagent.New(llmagent.Config{
			Name:                  "plan-judge",
			Description:           "independent DAG plan verifier",
			Model:                 judgeModel,
			Instruction:           planRubricInstruction,
			Tools:                 []tool.Tool{submit},
			GenerateContentConfig: judgeGenConfig(maxOutputTokens, thinkingLevel),
		})
		if err != nil {
			return false, "", fmt.Errorf("vetting: build plan judge agent: %w", err)
		}
		r, err := runner.New(runner.Config{
			AppName: "quack-plan-judge", Agent: judgeAgent,
			SessionService: session.InMemoryService(), AutoCreateSession: true,
		})
		if err != nil {
			return false, "", fmt.Errorf("vetting: plan judge runner: %w", err)
		}

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		prompt := buildPlanJudgePrompt(request, planSummary, memSection)
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}
		var repeats repeatLoopDetector
		chatID := ledger.CoordsFromContext(runCtx).ChatID
		for ev, rerr := range r.Run(runCtx, "plan-judge", judgeSessionID(chatID, "verdict"), content, adkagent.RunConfig{}) {
			if rerr != nil {
				return false, "", rerr
			}
			if ev == nil || ev.Content == nil {
				continue
			}
			for _, p := range ev.Content.Parts {
				if p != nil && p.Text != "" {
					repeats.observe(p.Text)
				}
			}
			// A runaway repeat loop must not decode the same text forever (#889).
			if repeats.tripped {
				slog.Warn("plan judge aborted: runaway repeat detected mid-generation", "component", "vetting")
				cancel()
				break
			}
		}
		if !submitted {
			return false, "", fmt.Errorf("vetting: plan judge ended without a verdict")
		}
		return sink.Accept, sink.Reason, nil
	}
}

// buildPlanJudgePrompt: assembles the plan judge's user message. memSection
// is "" when no project memory matched (epic #1255 P2).
func buildPlanJudgePrompt(request, planSummary, memSection string) string {
	var sb strings.Builder
	if memSection != "" {
		sb.WriteString(memSection)
	}
	sb.WriteString("User's request:\n")
	sb.WriteString(strings.TrimSpace(request))
	sb.WriteString("\n\nProposed plan:\n")
	sb.WriteString(strings.TrimSpace(planSummary))
	sb.WriteString("\n\nIs this plan acceptable? Call submit_plan_verdict now.")
	return sb.String()
}
