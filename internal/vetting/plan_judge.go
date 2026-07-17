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
// user's request — the replacement for the planner's old regex routing
// backstop (checkImplementationRouting). request is the user's verbatim ask,
// planSummary a human-readable rendering of the proposed nodes (id, agent,
// task, dependencies). Returns accept + a reason naming what, specifically, is
// wrong when rejecting (so the orchestrator's own re-plan loop can fix it).
type PlanJudge func(ctx context.Context, request, planSummary string) (accept bool, reason string, err error)

// submitPlanVerdictTool is the structured-termination tool the plan judge
// calls to record its verdict and end its run — mirrors safety_judge.go's
// submit_safety_verdict / judge.go's submit_verdict pattern.
const submitPlanVerdictTool = "submit_plan_verdict"

type planVerdictArgs struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason"`
}

// planRubricInstruction is the judge's whole system prompt: a small, fixed
// five-criterion rubric (deliberately NOT the worker's G-Eval rubric — this
// judges the SHAPE of a plan, not the quality of an answer) that replaces the
// old verb/delivery-term regex. Intent is read from the user's actual request
// against the proposed plan, not from string-matching the request's prose, so
// a plan-only run ("write up a plan; the issue body's acceptance text happens
// to say 'open a PR'") is no longer mistaken for an implement-and-deliver run.
const planRubricInstruction = `You are an independent reviewer judging a PROPOSED PLAN — a DAG of agent nodes about to run — against the user's actual request. You did not author the plan, you have no tools, and you are not judging any node's output; the nodes have not run yet.

You will be shown the user's request and the proposed plan: each node's id, agent, task, and dependencies, plus the plan's declared setup (working clone + branch) and delivery (how the result reaches GitHub) — or "(none declared)" for either.

A plan is ACCEPTABLE only when ALL seven hold:
1. Addresses the ask — the plan solves the user's actual request, not a tangent.
2. Right terminal deliverable for the request type — a planning/plan-only request (the deliverable is a plan or document) needs NO code-implementer node; an implement-and-deliver request (the deliverable is shipped code) needs a terminal code-implementer node that clones the repo, implements the change with tests, runs checks, and commits LOCALLY, then stages the delivery — the plan's declared delivery pushes the branch and opens the PR after the trust gate; the node itself never pushes or opens the PR; a review request needs a code-reviewer node; a pure-research/plan request stays read-only with no delivery node at all.
3. Grounded — the plan is specific to this request (names real files/conventions where known, or has a node establish them), not generic boilerplate that could apply to any repo.
4. Minimal, correct decomposition — no obvious over- or under-splitting, and a repo-analysis/exploration node is never the plan's terminal (last, undepended-on) deliverable.
5. Verifiable — where the plan changes code, some node's task says how the change will be checked (tests/checks), not just that it will be written.
6. Setup + delivery match the request type — a plan with NO GitHub repo involved at all (plain research/chat) may declare neither. Otherwise: an implement-and-deliver request needs delivery.kind "pull_request" AND setup.work_branch set (the harness needs a branch to commit onto); a review request needs delivery.kind "review"; a plan-only/research request against a repo needs delivery.kind "comment". A plan that touches a repo but declares neither, or declares the wrong kind for its request type, fails this criterion.
7. Independent, goal-scoped decomposition — when the plan implements code, it is a CHAIN of code-implementer nodes wired by depends_on, each covering ONE logical, independent portion of work with a clear, articulable goal — not one monolithic implementer node asked to do everything, and not arbitrarily sliced (e.g. by a commit-count target). Node N builds on node N-1's committed work through the shared clone. A portion whose goal can't be stated in one sentence is really two portions. Treat unusual size or complexity as a SIGNAL, not the axis: a portion so large or tangled that it can't be reviewed and tested as one coherent unit is probably more than one goal in disguise — under-decomposed, and should fail this criterion. A node may make as many atomic commits as its own portion naturally needs — that granularity belongs to the implementer, not the plan.

Read the REQUEST TYPE carefully before judging criteria 2 and 6 — the most common mistake is forcing a code-implementer (or a "pull_request" delivery) onto a plan-only or research-only request just because its acceptance text happens to mention "PR" or "commit" (e.g. an issue whose body describes what the EVENTUAL implementation PR should look like, while THIS request only asks for the plan). Judge what THIS request is actually asking the plan to DO, not every verb it contains.

Give the plan the benefit of the doubt on ambiguous phrasing — this check exists to catch a plan that is CLEARLY the wrong shape for the request (implement-and-deliver routed to research-only, or vice versa; a monolithic implementer where the feature obviously decomposes into several independent portions), not to nitpick a reasonable decomposition.

Call submit_plan_verdict exactly once with accept (bool) and reason (if rejecting: name the ONE specific edit that fixes it — e.g. "add a terminal code-implementer node", "this is a plan-only request; remove the code-implementer node", "set delivery.kind to pull_request and setup.work_branch to a branch name", or "split the implementer into a chain of independent, goal-scoped nodes" — never a vague "reconsider the plan"; if accepting: a brief reason is fine, or "").`

// NewPlanJudge returns a PlanJudge backed by judgeModel — deliberately the
// SAME judge model/provider as the trust gate's judge stage (gates.judge),
// reused rather than provisioned separately: a plan-routing verdict is one
// cheap, tool-less call, so it costs nothing extra to piggyback on the model
// already resident for node judging. Each call is a single-shot, fully
// ISOLATED agent run (its own in-memory session — mirrors judge.go/
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
