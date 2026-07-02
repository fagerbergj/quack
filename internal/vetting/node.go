package vetting

import (
	"fmt"
	"log/slog"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// NewGatedWorkerNode re-expresses the trust gate as a native ADK v2 workflow
// node: a first-class graph node whose body is a dynamic refine loop
// (worker → deterministic checks → judge → revise) that reuses judge.go's
// evaluation logic. It is the v2 replacement for the custom-agent gate (gate.go).
//
// Unlike the legacy gate it does NOT manipulate the worker's session view or emit
// orphan marker-FunctionResponses. Those markers are exactly what v2's
// contents_processor rejects ("no function call event found for function
// responses ids"), and the critiqueContext/filteredSession machinery that hid
// them from v1.4.0 no longer works. Instead:
//   - the worker runs as an AgentNode via RunNode, in its own sub-branch, so its
//     model request is clean and its thinking/tool events flow natively on the
//     workflow stream (the translator turns them into SSE — Phase 4);
//   - tool activity for deterministic citation scoring is reconstructed from the
//     session after each worker run (activityFromSession), not intercepted live.
//
// The node input is the task string (its round-0 prompt); its output is the
// vetted answer. Placed as a FIRST-CLASS node it is durably skipped on resume
// (a completed node is not re-run — see .quack/adk2-spike-findings.md), which the
// spike proved dynamic RunNode children are NOT.
//
// ponytail: self-critique (old Stage 1) is dropped — the advisor consult replaces
// it (a later increment). The loop is worker → deterministic → judge → revise.
func NewGatedWorkerNode(name string, worker adkagent.Agent, judge JudgeFactory, cfg Config) (workflow.Node, error) {
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		return nil, err
	}
	fn := func(ctx adkagent.Context, task string, _ func(*session.Event) error) (string, error) {
		// First-class node: as the graph entry its input is the user content; as a
		// mid-graph node its input is the predecessor's output. Fall back to the
		// session's user content if the typed input arrives empty.
		if strings.TrimSpace(task) == "" {
			task = contentPlainText(ctx.UserContent())
		}
		return RunGatedRefine(ctx, workerNode, judge, cfg, task)
	}
	return workflow.NewDynamicNode[string, string](name, fn, workflow.NodeConfig{}), nil
}

// NewWorkerNode wraps a worker agent as an AgentNode for use as the worker inside
// a gated refine loop (see RunGatedRefine).
func NewWorkerNode(worker adkagent.Agent) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(worker, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("vetting: build worker node: %w", err)
	}
	return n, nil
}

// RunGatedRefine runs the trust-gate refine loop against an already-built worker
// node: worker draft → deterministic citation/length checks → independent judge →
// revise, until the score clears cfg.Threshold or cfg.JudgeRounds is exhausted.
// It is the reusable core shared by NewGatedWorkerNode and the DAG graph builder
// (dag.BuildWorkflow); callable from inside any dynamic-node body since it drives
// the worker via RunNode. prompt is the fully-assembled worker instruction.
func RunGatedRefine(ctx adkagent.Context, workerNode workflow.Node, judge JudgeFactory, cfg Config, prompt string) (string, error) {
	log := slog.With("component", "vetting")
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

	answer, err := runWorkerNode(ctx, workerNode, prompt, 0)
	if err != nil {
		return "", err
	}

	// Judge/revise loop: judge the current answer, fold in the deterministic
	// citation/length criteria, and on a fail revise via a fresh worker call whose
	// prompt inlines the feedback + prior answer (buildRevisionContent is
	// self-contained, so the stateless worker needs no session continuity).
	for round := 1; judge != nil && round <= cfg.JudgeRounds; round++ {
		if strings.TrimSpace(answer) == "" {
			break // nothing to judge; empty-answer recovery is a later increment
		}
		act := activityFromSession(ctx.Session())
		v, jerr := runJudgeAgent(ctx, judge, cfg, question, answer, func(*genai.Part) bool { return true })
		if jerr != nil {
			log.Warn("judge round error; surfacing answer unvetted", "round", round, "err", jerr)
			return answer, nil
		}
		v = foldDeterministic(v, answer, act)
		passed := v.Score >= cfg.Threshold
		log.Info("judge round done", "round", round, "score", v.Score, "passed", passed)
		if passed || round >= cfg.JudgeRounds {
			break
		}
		revisePrompt := contentPlainText(buildRevisionContent(cfg.Constitution, question, answer, v.Feedback, act))
		revised, rerr := runWorkerNode(ctx, workerNode, revisePrompt, round)
		if rerr != nil {
			return answer, nil // revision failed; keep the prior answer
		}
		if strings.TrimSpace(revised) != "" {
			answer = revised
		}
	}
	return answer, nil
}

// runWorkerNode runs the worker as a sub-branched child with a stable per-round
// RunID (so a completed round replays from the event log on resume rather than
// re-executing) and returns its answer with leaked <think> content stripped.
func runWorkerNode(ctx adkagent.Context, workerNode workflow.Node, input string, round int) (string, error) {
	out, err := workflow.RunNode[string](ctx, workerNode, input,
		workflow.WithUseSubBranch(), workflow.WithRunID(fmt.Sprintf("worker-r%d", round)))
	if err != nil {
		return "", err
	}
	return stream.StripThinking(out), nil
}

// foldDeterministic folds the code-owned criteria (citation backing, answer
// length) into the judge's verdict and re-aggregates (weakest-link). Mirrors the
// deterministic fold in gate.run's judge stage.
func foldDeterministic(v verdict, answer string, act workerActivity) verdict {
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	if ls := lengthScore(answer); ls < 1.0 {
		v.Criteria["sufficient_length"] = criterionScore{Score: ls, Reason: fmt.Sprintf("deterministic: %d chars", len(strings.TrimSpace(answer)))}
	}
	if det, details, hasCites := citationScore(answer, act); hasCites {
		v.Criteria["cites_sources"] = criterionScore{Score: det, Reason: fmt.Sprintf("deterministic: %d cited URL(s), mean backing %.2f", len(details), det)}
	}
	return aggregateVerdict(v)
}

// activityFromSession reconstructs the worker's retrieval activity (web_search
// queries, fetched URLs, searched URLs) from the workflow session events. It
// replaces the legacy live-stream capture in gate.runWorker: web_fetch calls are
// paired to their responses by call ID, and web_search results feed the "seen"
// set. Consumed by the deterministic citation check.
func activityFromSession(sess session.Session) workerActivity {
	act := workerActivity{fetched: map[string]fetchRecord{}, seen: map[string]string{}}
	if sess == nil {
		return act
	}
	pending := map[string]string{} // web_fetch call ID → URL
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				switch p.FunctionCall.Name {
				case "web_search":
					if q, ok := p.FunctionCall.Args["query"].(string); ok && strings.TrimSpace(q) != "" {
						act.searches = append(act.searches, strings.TrimSpace(q))
					}
				case "web_fetch":
					if u, ok := p.FunctionCall.Args["url"].(string); ok && strings.TrimSpace(u) != "" {
						pending[p.FunctionCall.ID] = strings.TrimSpace(u)
					}
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_fetch" {
				if url, known := pending[p.FunctionResponse.ID]; known {
					delete(pending, p.FunctionResponse.ID)
					if result, ok := p.FunctionResponse.Response["result"].(string); ok && strings.TrimSpace(result) != "" {
						act.fetched[url] = fetchRecord{sample: strings.TrimSpace(trimToSample(result))}
					}
				}
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_search" {
				recordSearchResults(act.seen, p.FunctionResponse.Response)
			}
		}
	}
	return act
}
