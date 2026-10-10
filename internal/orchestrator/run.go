package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadartifactstool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

const orchRunID = "orchestrator"

// orchRun is the per-turn state Run assembles; each build/invoke/finish method handles one phase.
type orchRun struct {
	o           *Orchestrator
	ctx         context.Context
	userID      string
	sessionID   string
	source      string
	message     string
	attachments []*genai.Part

	safeYield    func(stream.SSEEvent, error) bool
	planCache    *tools.PlanCache
	guardStopped *atomic.Bool
	history      []dag.HistoryTurn

	toolList   []tool.Tool
	toolsets   []tool.Toolset
	hooks      *tools.Hooks
	memSvc     adkmemory.Service
	artifacts  artifact.Service
	runner     *runner.Runner
	translator *stream.Translator
}

// buildDagTools: the planRC + the five DAG tools. Returns the message to
// yield (stream.Errorf) on failure, "" on success.
func (s *orchRun) buildDagTools(githubSetup *dag.Setup) string {
	// The dag_node/dag_plan records ARE the plan's only state, so planning
	// must still work with no artifact service configured (a test, a degraded deploy).
	recordSvc := s.o.artifacts
	if recordSvc == nil {
		recordSvc = artifact.InMemoryService()
	}
	planRC := recordstore.New(recordSvc, artifactref.AppName, s.userID, s.sessionID)
	if s.o.ledgerStore != nil {
		planRC = planRC.WithLedger(s.o.ledgerStore)
	}
	nodeIsRunning := func(nodeID string) bool { return s.o.executor.NodeIsLive(s.sessionID, nodeID) }
	allowedKinds := tools.AllowedDeliveryKindsFromContext(s.ctx)
	listNodesTool, err := tools.NewListNodesTool(planRC, nodeIsRunning)
	if err != nil {
		return "orchestrator: list_nodes tool: " + err.Error()
	}
	// The pinned roster's names: a reload mid-turn must not offer agents this run can't dispatch.
	agentNames := dag.AgentNamesFor(s.ctx)
	createPlanTool, err := tools.NewCreatePlanTool(planRC, orchestratorName, githubSetup, nodeIsRunning, allowedKinds, s.o.assignmentMeta, agentNames)
	if err != nil {
		return "orchestrator: create_plan tool: " + err.Error()
	}
	editPlanTool, err := tools.NewEditPlanTool(planRC, orchestratorName, githubSetup, nodeIsRunning, allowedKinds, s.o.assignmentMeta, agentNames)
	if err != nil {
		return "orchestrator: edit_plan tool: " + err.Error()
	}
	runStep := func(stepCtx context.Context, plan dag.Plan, seeded map[string]string, run map[string]bool) (map[string]string, map[string]bool, map[string]bool, error) {
		return s.o.executor.RunPlanStep(stepCtx, plan, AppName, s.userID, s.sessionID, seeded, run)
	}
	finalizeStep := func(stepCtx context.Context, plan dag.Plan, outputs map[string]string) string {
		return s.o.finalizeAnswer(stepCtx, plan, outputs, s.sessionID, nil)
	}
	execTool, err := tools.NewExecuteTool(s.o.planner, planRC, s.planCache, s.o.executor.Provision, runStep, finalizeStep, s.history, s.message, s.attachments,
		githubSetup, allowedKinds,
		tools.WorkerAskFromContext(s.ctx), tools.ContextItemsFromContext(s.ctx), tools.PlanOnlyFromContext(s.ctx),
		orchestratorName, s.o.assignmentFreshness)
	if err != nil {
		return "orchestrator: execute tool: " + err.Error()
	}
	choiceTool, err := tools.NewGetUserChoiceTool()
	if err != nil {
		return "orchestrator: choice tool: " + err.Error()
	}
	s.toolList = []tool.Tool{listNodesTool, createPlanTool, editPlanTool, execTool, choiceTool}
	return ""
}

// buildMemoryArtifactTools: the memory and artifact tools appended after the
// DAG tools. Returns the message to yield on failure, "" on success.
func (s *orchRun) buildMemoryArtifactTools(githubSetup *dag.Setup) string {
	if s.o.userMem != nil {
		commitTool, err := tools.NewCommitMemoryTool(s.o.userMem, s.userID, s.sessionID, s.source)
		if err != nil {
			return "orchestrator: commit_memory tool: " + err.Error()
		}
		s.toolList = append(s.toolList, memory.NewPreload(), commitTool)
		s.memSvc = s.o.userMem.View(memory.Scope{User: s.userID, Legacy: s.userID}, nil)
	}
	if s.o.taskMem != nil {
		// ponytail: repo scope from the dispatch's known origin when
		// present; user-only ceiling remains for chats with no origin.
		recallSc := memory.Scope{User: s.userID, Legacy: s.userID}
		if githubSetup != nil {
			recallSc.Repo = workspace.NormalizeRepoURL(githubSetup.Repo)
		}
		recallTool, err := tools.NewRecallMemoryTool(s.o.taskMem, recallSc, s.o.ledgerStore, s.sessionID)
		if err != nil {
			return "orchestrator: recall_memory tool: " + err.Error()
		}
		s.toolList = append(s.toolList, recallTool)
	}
	if s.o.artifacts != nil {
		s.toolList = append(s.toolList, loadartifactstool.New())
		s.artifacts = failSoftListArtifacts{s.o.artifacts}
		rc := recordstore.New(s.o.artifacts, artifactref.AppName, s.userID, s.sessionID)
		if s.o.ledgerStore != nil {
			rc = rc.WithLedger(s.o.ledgerStore)
		}
		if s.o.schemas != nil {
			rc = rc.WithSchemas(s.o.schemas)
		}
		listTool, err := tools.NewListArtifactsTool(rc)
		if err != nil {
			return "orchestrator: list_artifacts tool: " + err.Error()
		}
		editTool, err := tools.NewEditArtifactTool(rc, orchestratorName, &tools.RoundCoords{})
		if err != nil {
			return "orchestrator: edit_artifact tool: " + err.Error()
		}
		// Split like BuildNativeArtifactTools: write_artifact needs DocumentHint,
		// write_<kind> (code_review) needs SubjectHint - one shared hint mismatches one of the two.
		writeTool, err := tools.NewWriteArtifactTool(rc, orchestratorName, &tools.RoundCoords{}, vetting.DocumentHint(s.sessionID))
		if err != nil {
			return "orchestrator: write_artifact tool: " + err.Error()
		}
		s.toolList = append(s.toolList, listTool, editTool, writeTool)
		writeKindTools, err := tools.NewWriteKindTools(rc, orchestratorName, &tools.RoundCoords{}, vetting.SubjectHint(s.sessionID))
		if err != nil {
			return "orchestrator: write_<kind> tools: " + err.Error()
		}
		s.toolList = append(s.toolList, writeKindTools...)
		if s.o.renderUI {
			renderTool, err := tools.NewRenderUITool(rc, orchestratorName, nil)
			if err != nil {
				return "orchestrator: render_ui tool: " + err.Error()
			}
			s.toolList = append(s.toolList, renderTool)
		}
	}
	return ""
}

// buildRunner: agent, agent node, single-node workflow, and the runner.
// Returns the message to yield on failure, "" on success.
func (s *orchRun) buildRunner() string {
	cfg := llmagent.Config{
		Name:        orchestratorName,
		Description: "Routes requests to the right specialist agents - web research, code implementation, media reading - and answers conversational queries directly.",
		Model:       s.o.model,
		InstructionProvider: func(rc adkagent.ReadonlyContext) (string, error) {
			return s.o.sysPrompt(rc), nil
		},
		Tools:    s.toolList,
		Toolsets: s.toolsets,
		Mode:     llmagent.ModeChat,
	}
	s.hooks.Wire(&cfg)
	ag, err := llmagent.New(cfg)
	if err != nil {
		return "orchestrator: build agent: " + err.Error()
	}
	agentNode, err := workflow.NewAgentNode(ag, workflow.NodeConfig{})
	if err != nil {
		return "orchestrator: agent node: " + err.Error()
	}
	wf, err := workflowagent.New(workflowagent.Config{
		Name:  "orchestrator-workflow",
		Edges: workflow.Chain(workflow.Start, agentNode),
	})
	if err != nil {
		return "orchestrator: workflow: " + err.Error()
	}
	r, err := runner.New(runner.Config{
		AppName:           AppName,
		Agent:             wf,
		SessionService:    conversationSessions{s.o.sessions},
		MemoryService:     s.memSvc,
		ArtifactService:   s.artifacts,
		AutoCreateSession: true,
		// The long-lived chat session otherwise grows unbounded across turns.
		Compaction: s.o.compaction,
	})
	if err != nil {
		return "orchestrator: runner: " + err.Error()
	}
	s.runner = r
	return ""
}

// buildContent: the turn's user content - the message plus any attachment
// description, or the get_user_choice FunctionResponse when this turn replies a pending choice.
func (s *orchRun) buildContent(pending PendingQuestion, hasPending bool) *genai.Content {
	text := s.message
	if desc := dag.AttachmentDesc(s.attachments); desc != "" {
		text += "\n\n" + desc
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}
	if hasPending && pending.choiceCallID != "" {
		content = &genai.Content{Role: "user", Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       pending.choiceCallID,
				Name:     tools.ChoiceToolName,
				Response: map[string]any{tools.ChoiceAnswerKey: s.message},
			},
		}}}
	}
	return content
}

// invoke: one pass over the runner, translating and scoping every event to this run.
// produced reflects the model's output OR a plan selection; a pending (unselected) plan is not production.
func (s *orchRun) invoke(content *genai.Content) (produced, stop bool) {
	for ev, err := range s.runner.Run(s.ctx, s.userID, s.sessionID, content, adkagent.RunConfig{}) {
		if err != nil {
			s.safeYield(stream.Errorf(err.Error()), nil)
			return false, true
		}
		if ev == nil {
			continue
		}
		if turnProduced(ev) {
			produced = true
		}
		for _, se := range s.translator.Event(ev) {
			if !s.safeYield(stream.ScopeToRun(se, orchRunID), nil) {
				return produced, true
			}
		}
	}
	if _, selected := s.planCache.Selected(); selected {
		produced = true
	}
	if _, _, tripped := s.planCache.LoopGuard(); tripped {
		produced = true // the turn hands the user a choice; no continuation nudge
	}
	return produced, false
}

// emitAgentComplete: the run's usage/agent_complete event.
func (s *orchRun) emitAgentComplete() {
	model, promptTokens, completionTokens, reasoningTokens, totalTokens, cachedTokens, finishReason := s.translator.Usage()
	s.safeYield(stream.SSEEvent{Name: stream.EventAgentComplete, Data: stream.AgentCompleteData{
		RunID: orchRunID, Stage: stream.StageWorker,
		Model: model, PromptTokens: promptTokens, CompletionTokens: completionTokens,
		ReasoningTokens: reasoningTokens, TotalTokens: totalTokens, CachedTokens: cachedTokens, FinishReason: finishReason,
		FinishedAtMs: time.Now().UnixMilli(),
	}}, nil)
}

// handlePlanExhaustion fails a run whose planning exhausted its rejection budget; one rejection is
// normal iteration and a pending question a legitimate stop. True = the turn terminated here.
func (s *orchRun) handlePlanExhaustion() bool {
	if _, selected := s.planCache.Selected(); !selected {
		if count, reason := s.planCache.Rejections(); count >= minRejectionsForExhaustion {
			if _, hasPending := s.o.PendingQuestion(s.ctx, s.userID, s.sessionID); !hasPending {
				slog.Error("planning exhausted its rejection budget without an acceptable plan; suppressing the judge's internal rejection text from the reply",
					"component", "orchestrator", "chat", s.sessionID, "rejections", count, "reason", reason)
				s.safeYield(stream.Errorf(planExhaustedNotice), nil)
				s.o.persistAnswer(s.ctx, s.userID, s.sessionID, planExhaustedNotice)
				s.safeYield(stream.Done(), nil)
				return true
			}
		}
	}
	return false
}

// Plan loop choice: the options offered once the plan judge keeps rejecting the same plan.
const (
	planLoopChoicePrefix = "plan-loop-"
	planLoopRunAsIs      = "Run the plan as is"
	planLoopRephrase     = "Let me rephrase"
	planLoopReasonMax    = 300
)

// askAfterPlanLoop asks the user, through a get_user_choice call of the orchestrator's own, whether to
// run the plan the judge keeps rejecting; its id carries that plan's ShapeKey for the answer's turn.
func (s *orchRun) askAfterPlanLoop(reason, shapeKey string) {
	ctx := context.WithoutCancel(s.ctx)
	ev := session.NewEvent(ctx, "")
	ev.Author = orchestratorName
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: planLoopChoicePrefix + shapeKey + "-" + uuid.NewString(), Name: tools.ChoiceToolName,
		Args: map[string]any{"question": planLoopQuestion(s.source, reason), "options": []any{planLoopRunAsIs, planLoopRephrase}},
	}}}}
	if resp, err := s.o.sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: s.userID, SessionID: s.sessionID}); err == nil && resp != nil {
		if err := s.o.sessions.AppendEvent(ctx, resp.Session, ev); err != nil {
			slog.Warn("plan loop choice not persisted", "component", "orchestrator", "chat", s.sessionID, "err", err)
		}
	}
	for _, se := range s.translator.Event(ev) {
		s.safeYield(stream.ScopeToRun(se, orchRunID), nil)
	}
	s.emitAgentComplete()
	s.safeYield(stream.Done(), nil)
}

// planLoopQuestion names the options in its text (a GitHub comment shows only the question). Only
// the app shows the judge's reason, truncated: it can quote recalled memory.
func planLoopQuestion(source, reason string) string {
	reply := fmt.Sprintf("Reply %q to run it anyway, or %q.", planLoopRunAsIs, planLoopRephrase)
	if source != SourceApp {
		return "The plan reviewer rejected this plan repeatedly. " + reply
	}
	line := []rune(strings.Join(strings.Fields(reason), " "))
	if len(line) > planLoopReasonMax {
		line = append(line[:planLoopReasonMax], '…')
	}
	return "The plan reviewer rejected this plan repeatedly: " + string(line) + "\n\n" + reply
}

// waivedPlanShape is the ShapeKey this turn's answer to the plan loop choice waives the judge for, or
// "". Contains, not equals: a GitHub reply arrives wrapped in its envelope.
func waivedPlanShape(pending PendingQuestion, hasPending bool, message string) string {
	rest, ok := strings.CutPrefix(pending.choiceCallID, planLoopChoicePrefix)
	if !hasPending || !ok || !strings.Contains(strings.ToLower(message), strings.ToLower(planLoopRunAsIs)) {
		return ""
	}
	key, _, _ := strings.Cut(rest, "-")
	return key
}

// finishLoop handles every terminal outcome after the first invoke. Returns the attempt count and
// whether the turn ended.
func (s *orchRun) finishLoop(produced, stop bool) (int, bool) {
	attempts := 1
	// A hard-stopped turn reproduces the identical loop on an unchanged retry, so give up now.
	for attempt := 1; !produced && !stop && !s.guardStopped.Load() && attempt <= maxOrchestratorContinues; attempt++ {
		slog.Warn("orchestrator turn produced no plan and no answer; continuing it",
			"component", "orchestrator", "chat", s.sessionID, "attempt", attempt)
		produced, stop = s.invoke(continuationContent())
		attempts++
	}
	if stop {
		return attempts, true
	}
	if !produced && s.guardStopped.Load() {
		slog.Error("orchestrator turn ended by the repeat guard's hard stop; not retrying it unchanged",
			"component", "orchestrator", "chat", s.sessionID, "attempts", attempts)
		s.safeYield(stream.Errorf("The orchestrator got stuck repeating the same malformed tool call and stopped. "+
			"Please try again or rephrase your request."), nil)
		return attempts, true
	}
	if reason, key, tripped := s.planCache.LoopGuard(); tripped {
		s.askAfterPlanLoop(reason, key)
		return attempts, true
	}
	s.emitAgentComplete()
	if !produced {
		slog.Error("orchestrator produced no plan and no answer; giving up",
			"component", "orchestrator", "chat", s.sessionID, "attempts", attempts)
		s.safeYield(stream.Errorf("The orchestrator ended its turn without a plan or an answer, "+
			"even after being asked to continue. Nothing was run. Please try again."), nil)
		return attempts, true
	}
	return attempts, s.handlePlanExhaustion()
}
