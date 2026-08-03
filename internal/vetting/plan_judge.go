package vetting

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// PlanJudge decides whether a proposed DAG plan is a well-formed answer to the
// user's request - the replacement for the planner's old regex routing
// backstop (checkImplementationRouting). request is the user's verbatim ask,
// planSummary a human-readable rendering of the proposed nodes (id, agent,
// task, dependencies). Returns accept + a reason naming what, specifically, is
// wrong when rejecting (so the orchestrator's own re-plan loop can fix it).
type PlanJudge func(ctx context.Context, request, planSummary string) (accept bool, reason string, err error)

// submitPlanVerdictTool is the structured-termination tool the plan judge
// calls to record its verdict and end its run - mirrors safety_judge.go's
// submit_safety_verdict / judge.go's submit_verdict pattern.
const submitPlanVerdictTool = "submit_plan_verdict"

type planVerdictArgs struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason"`
}

// planRubricInstruction is the judge's whole system prompt: a small, fixed
// five-criterion rubric (deliberately NOT the worker's G-Eval rubric - this
// judges the SHAPE of a plan, not the quality of an answer) that replaces the
// old verb/delivery-term regex. Intent is read from the user's actual request
// against the proposed plan, not from string-matching the request's prose, so
// a plan-only run ("write up a plan; the issue body's acceptance text happens
// to say 'open a PR'") is no longer mistaken for an implement-and-deliver run.
const planRubricInstruction = `You are an independent reviewer judging a PROPOSED PLAN - a DAG of agent nodes about to run - against the user's actual request. You did not author the plan, you have no tools, and you are not judging any node's output; the nodes have not run yet.

You will be shown the user's request and the proposed plan: each node's id, agent, task, and dependencies, plus the plan's declared setup (working clone + branch) and delivery (how the result reaches GitHub) - or "(none declared)" for either.

A plan is ACCEPTABLE only when ALL five hold:
1. Delivers what was asked - read the request, then ask: if this plan runs exactly as written, does it hand back what the request asked to receive? Name the artifact the request wants (a plan, a review, shipped code, an answer, a document), find the node whose own task actually produces it, and confirm that node is the plan's terminal (last, undepended-on) node - not merely a step feeding one. A terminal node that only explores, investigates, or reports never satisfies a request whose deliverable is a plan, a review, or shipped code, however thorough its report is - the live failure this guards against: a single code-explorer node tasked to "produce a detailed report" was accepted for a request that asked for an implementation plan (approach, files to change, how to verify). The reverse also fails: a plan that stops at exploring or reviewing when the request asked for shipped code. Judge setup and delivery the same way - do they match what the plan's nodes actually do and what getting the result back to the user requires - by reasoning about THIS request and THIS plan, not by matching a catalogue of request types.
2. Grounded - the plan is specific to this request (names real files/conventions where known, or has a node establish them), not generic boilerplate that could apply to any repo.
3. Minimal, correct decomposition - no obvious over- or under-splitting, and is sized to the SCOPE the request itself sets: when the request names specific commits, threads, files, or questions ("verify commit X resolves the finding", "a scoped re-check of those three threads - NOT a full re-review", "just check the auth changes"), the plan must match THAT scope, not the whole PR/repo - inflating a narrow ask into a full review or re-implementation fails this criterion exactly like ignoring part of a broad ask does, and this scope check is never a license to expand past what was asked just because the surrounding PR or repo is large.
4. Verifiable - where the plan changes code, some node's task names how the change will be checked in concrete terms: specific test file(s) or test target(s) to add/run, and the actual runnable command(s) (or, when the exact command can't be known before cloning, an instruction to derive and run the repo's real build/lint/test commands) - not a vague "add tests" or "verify it works" with no named target.
5. Independent, goal-scoped decomposition - decompose by independent PORTION of work with a clear, articulable goal, NEVER by activity or layer. A single cohesive change - one goal, stated in one sentence - is CORRECTLY a SINGLE code-implementer node, even though that node's own job includes implementing the API and the logic, writing its own tests, and running checks: splitting "API implementation" vs. "logic implementation" vs. "testing/verification" vs. "run checks" vs. "commit" into separate nodes for what is really one goal FAILS this criterion just as surely as a monolithic node covering two unrelated goals does. Require a CHAIN of code-implementer nodes wired by depends_on only when the request genuinely contains multiple INDEPENDENT portions - distinct goals that could be pursued, reviewed, and tested separately - with node N building on node N-1's committed work through the shared clone. A portion whose goal can't be stated in one sentence is really two portions and belongs in its own node; treat unusual size or complexity as a SIGNAL of that, not the axis itself - a portion so large or tangled it can't be reviewed and tested as one coherent unit is probably more than one goal in disguise, under-decomposed, and should fail this criterion. A node may make as many atomic commits as its own portion naturally needs - that granularity, like its tests, belongs to the implementer, not the plan.

Read what the request actually asks to RECEIVE before judging criterion 1 - the most common mistake is inferring the deliverable from a verb the request happens to contain rather than what it explicitly asks for (e.g. an issue whose body describes what the EVENTUAL implementation PR should look like, while THIS request only asks for the plan). Judge what THIS request asks the plan to hand back, not every verb it contains.

Give the plan the benefit of the doubt on ambiguous phrasing - this check exists to catch a plan that is CLEARLY the wrong shape for the request (a terminal exploration/report node standing in for a requested plan, review, or implementation, or vice versa; a monolithic implementer where the feature obviously decomposes into several independent portions; a single cohesive change pointlessly split into activity-scoped nodes; or an explicitly narrowed ask forced into the large-diff fan-out pattern it never asked for), not to nitpick a reasonable decomposition.

Call submit_plan_verdict exactly once with accept (bool) and reason (if rejecting: name the ONE specific edit that fixes it - e.g. "add a terminal node that actually writes the plan - the current terminal node only explores", "this is a plan-only request; drop the code-implementer node", "declare delivery so the shipped code can reach GitHub", "split the implementer into a chain of independent, goal-scoped nodes", or "shrink this to the commit/threads/files the user actually named instead of the whole PR" - never a vague "reconsider the plan"; if accepting: a brief reason is fine, or "").`

// NewPlanJudge returns a PlanJudge backed by judgeModel - deliberately the
// SAME judge model/provider as the trust gate's judge stage (gates.judge),
// reused rather than provisioned separately: a plan-routing verdict is one
// cheap, tool-less call, so it costs nothing extra to piggyback on the model
// already resident for node judging. Each call is a single-shot, fully
// ISOLATED agent run (its own in-memory session - mirrors judge.go/
// safety_judge.go's isolated-runner pattern). A judge that ends without
// calling submit_plan_verdict, or any run error, is returned as an error; the
// caller (dag.Planner.Build) degrades gracefully rather than wedging the run.
func NewPlanJudge(judgeModel model.LLM) PlanJudge {
	return func(ctx context.Context, request, planSummary string) (bool, string, error) {
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
			Name:        "plan-judge",
			Description: "independent DAG plan verifier",
			Model:       judgeModel,
			Instruction: planRubricInstruction,
			Tools:       []tool.Tool{submit},
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

		prompt := buildPlanJudgePrompt(request, planSummary)
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}
		for _, rerr := range r.Run(ctx, "plan-judge", "verdict", content, adkagent.RunConfig{}) { //nolint:staticcheck // ev unused; loop is only for side effects + error
			if rerr != nil {
				return false, "", rerr
			}
		}
		if !submitted {
			return false, "", fmt.Errorf("vetting: plan judge ended without a verdict")
		}
		return sink.Accept, sink.Reason, nil
	}
}

// buildPlanJudgePrompt assembles the plan judge's single user message.
func buildPlanJudgePrompt(request, planSummary string) string {
	var sb strings.Builder
	sb.WriteString("User's request:\n")
	sb.WriteString(strings.TrimSpace(request))
	sb.WriteString("\n\nProposed plan:\n")
	sb.WriteString(strings.TrimSpace(planSummary))
	sb.WriteString("\n\nIs this plan acceptable? Call submit_plan_verdict now.")
	return sb.String()
}
