// experiment.go: `quack experiment run`. --prompt pins the in-process resolver to one
// Langfuse version through langfuse.PinnedSource (cmd/quack/experiment.go wires it).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ExperimentOpts selects the dataset/agent/prompt version for `quack experiment run`.
type ExperimentOpts struct {
	Dataset string
	Agent   string
	Prompt  string // "system/<agent>@N": the pinned version every item's node must have run
	RunName string
	Limit   int
}

// ExperimentResult is one dataset item's run, printed in the summary table.
type ExperimentResult struct {
	ItemID   string
	TraceID  string
	Prompt   string
	Duration time.Duration
	Error    string
}

// experimentItemInput mirrors dataset export's datasetItemInput - only Task is read back.
type experimentItemInput struct {
	Task string `json:"task"`
}

// itemRun is one item's node run: the trace RunExperiment links, and what it scores on that trace.
type itemRun struct {
	Answer  string
	TraceID string
	// Prompt is the system/<agent> artifact the node resolved; zero when its prompt isn't one.
	Prompt ledger.ArtifactRef
	// Criteria: the gate's final judged round, each score on its criterion's rubric scale.
	Criteria []criterionScore
	Passed   bool
	Cost     itemCost
}

type criterionScore struct {
	Name        string
	Score       float64
	Explanation string
}

// itemCost is the node's spend: equal quality at a lower cost is a better prompt too.
type itemCost struct {
	bundle.Activity
	InputTokens, OutputTokens, CachedTokens int64
	JudgeRounds                             int
	WallTime                                time.Duration
}

// prompt is Prompt as the summary prints it and --prompt pins it: name@version, or "(static)" off disk.
func (r itemRun) prompt() string {
	switch {
	case r.Prompt.Name == "":
		return ""
	case r.Prompt.Source == artifactsrc.StaticSource:
		return r.Prompt.Name + " (static)"
	}
	return r.Prompt.Name + "@" + r.Prompt.VersionID
}

// itemRunner runs one item's task; it's the seam that lets RunExperiment be tested without booting serve.
type itemRunner interface {
	RunItem(ctx context.Context, task string) (itemRun, error)
}

// RunExperiment runs opts.Agent's node against every item in opts.Dataset outside any live
// GitHub event, via runner (see itemRunner), reporting each as a Langfuse dataset run item.
func RunExperiment(ctx context.Context, errOut io.Writer, runner itemRunner, lf *langfuse.Client, opts ExperimentOpts) ([]ExperimentResult, error) {
	items, err := listDatasetItems(ctx, lf, opts.Dataset, opts.Limit)
	if err != nil {
		return nil, err
	}

	var results []ExperimentResult
	for _, item := range items {
		res, err := runExperimentItem(ctx, runner, lf, item, opts)
		if err != nil {
			// Print what already completed - a hard error here otherwise drops
			// it from the summary table/--as-json even though it ran.
			fmt.Fprint(errOut, FormatExperimentSummary(results))
			return results, err
		}
		results = append(results, res)
		fmt.Fprintf(errOut, "item %s: trace=%s prompt=%s duration=%s\n", res.ItemID, res.TraceID, res.Prompt, res.Duration)
	}
	return results, nil
}

// listDatasetItems pages until an empty or last page, stopping at limit (<= 0 means no cap).
func listDatasetItems(ctx context.Context, lf *langfuse.Client, dataset string, limit int) ([]langfuse.DatasetItem, error) {
	var out []langfuse.DatasetItem
	for page := 1; ; page++ {
		items, totalPages, err := lf.ListDatasetItems(ctx, dataset, page)
		if err != nil {
			return nil, fmt.Errorf("experiment run: list dataset %q items: %w", dataset, err)
		}
		if len(items) == 0 {
			break
		}
		out = append(out, items...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		if page >= totalPages {
			break
		}
	}
	return out, nil
}

func runExperimentItem(ctx context.Context, runner itemRunner, lf *langfuse.Client, item langfuse.DatasetItem, opts ExperimentOpts) (ExperimentResult, error) {
	task, err := itemTask(item)
	if err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: %w", item.ID, err)
	}
	start := time.Now()
	run, err := runner.RunItem(ctx, task)
	res := ExperimentResult{ItemID: item.ID, TraceID: run.TraceID, Prompt: run.prompt(), Duration: time.Since(start)}
	switch {
	case err != nil:
		res.Error = err.Error()
	case run.TraceID == "":
		res.Error = "no trace id recorded for the node run"
	case opts.Prompt != "" && run.prompt() != opts.Prompt:
		// A pin the node never resolved (a plugin agent's prompt is a bundle file, say) would compare the wrong version.
		res.Error = fmt.Sprintf("node ran prompt %q, not the pinned %s", run.prompt(), opts.Prompt)
	}
	if res.Error != "" {
		return res, nil
	}
	if err := recordRunItem(ctx, lf, opts, item.ID, run); err != nil {
		return ExperimentResult{}, err
	}
	return res, nil
}

func itemTask(item langfuse.DatasetItem) (string, error) {
	b, err := json.Marshal(item.Input)
	if err != nil {
		return "", err
	}
	var in experimentItemInput
	if err := json.Unmarshal(b, &in); err != nil {
		return "", err
	}
	if in.Task == "" {
		return "", fmt.Errorf("item %s: input has no task field", item.ID)
	}
	return in.Task, nil
}

// recordRunItem scores the trace before linking it, so a failed link leaves only scores on an unlinked
// trace. The prompt rides on the run (description and metadata); the answer already lives on the trace.
func recordRunItem(ctx context.Context, lf *langfuse.Client, opts ExperimentOpts, itemID string, run itemRun) error {
	if err := lf.Ingest(ctx, itemScores(run, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return fmt.Errorf("experiment run: item %s: score trace: %w", itemID, err)
	}
	req := langfuse.CreateDatasetRunItemRequest{
		DatasetItemID: itemID, RunName: opts.RunName, TraceID: run.TraceID,
		RunDescription: fmt.Sprintf("prompt=%s agent=%s", run.prompt(), opts.Agent),
		Metadata: map[string]string{"agent": opts.Agent, "prompt_name": run.Prompt.Name,
			"prompt_version": run.Prompt.VersionID, "prompt_source": run.Prompt.Source},
	}
	if err := lf.CreateDatasetRunItem(ctx, req); err != nil {
		return fmt.Errorf("experiment run: item %s: create run item: %w", itemID, err)
	}
	return nil
}

// itemScores is every numeric score RunExperiment puts on run's trace: one per judged criterion,
// gate_passed, and the cost.* signals. Langfuse averages each per dataset run, so two runs compare column by column.
func itemScores(run itemRun, ts string) []langfuse.IngestEvent {
	score := func(name string, v float64, comment string) langfuse.IngestEvent {
		return langfuse.NumericScore(run.TraceID, name, v, comment, ts)
	}
	out := make([]langfuse.IngestEvent, 0, len(run.Criteria)+9)
	for _, c := range run.Criteria {
		out = append(out, score(c.Name, c.Score, c.Explanation))
	}
	passed := 0.0
	if run.Passed {
		passed = 1
	}
	c := run.Cost
	return append(out,
		score("gate_passed", passed, ""),
		score("cost.input_tokens", float64(c.InputTokens), ""),
		score("cost.output_tokens", float64(c.OutputTokens), ""),
		score("cost.cached_tokens", float64(c.CachedTokens), ""),
		score("cost.model_turns", float64(c.ModelTurns), ""),
		score("cost.tool_calls", float64(c.ToolCalls), ""),
		score("cost.rejected_tool_calls", float64(c.RejectedToolCalls), ""),
		score("cost.judge_rounds", float64(c.JudgeRounds), ""),
		score("cost.wall_time_s", c.WallTime.Seconds(), ""),
	)
}

// LiveItemRunner runs each item as a fresh chat against Base, then reads Agent's node run back from the
// chat's recording and its dag_nodes row.
type LiveItemRunner struct {
	Base   string
	Store  *store.Store
	Agent  string
	Rubric vetting.RubricScaler
}

func NewLiveItemRunner(base string, st *store.Store, agent string, rubric vetting.RubricScaler) *LiveItemRunner {
	return &LiveItemRunner{Base: base, Store: st, Agent: agent, Rubric: rubric}
}

func (r *LiveItemRunner) RunItem(ctx context.Context, task string) (itemRun, error) {
	c := &Client{BaseURL: strings.TrimRight(r.Base, "/"), HTTP: &http.Client{}}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		return itemRun{}, fmt.Errorf("create chat: %w", err)
	}
	st := newStreamState()
	onEvent := func(ev SSEEvent) error { st.handle(ev, nil); return nil }
	if err := c.SendMessage(ctx, chatID, task, onEvent); err != nil {
		return itemRun{}, err
	}
	res := st.result(chatID)
	if res.Status == StatusFailed {
		return itemRun{}, fmt.Errorf("%s", res.Error)
	}

	body, err := c.FetchRecording(ctx, chatID)
	if err != nil {
		return itemRun{}, fmt.Errorf("chat %s: fetch recording: %w", chatID, err)
	}
	sess, err := sessionFromBundleBytes(body)
	if err != nil {
		return itemRun{}, fmt.Errorf("chat %s: load recording: %w", chatID, err)
	}
	runs := sess.NodeRuns(map[string]bool{r.Agent: true})
	for _, key := range sortedStreamKeys(runs) {
		if run := runs[key]; run.Answer != "" {
			return r.itemRunFor(ctx, chatID, sess, key.Node, run), nil
		}
	}
	// A run item with no node run would otherwise report an empty traceId and the
	// orchestrator's own answer instead of the agent's - report it as an error.
	return itemRun{}, fmt.Errorf("chat %s: agent %q produced no node run", chatID, r.Agent)
}

// itemRunFor assembles nodeID's run: prompt and judge scores from the recording, tokens and timing from its
// dag_nodes row (the node_done totals); TraceID stays "" until that row exists, so the item reports rather than fails.
func (r *LiveItemRunner) itemRunFor(ctx context.Context, chatID string, sess *bundle.Session, nodeID string, nr bundle.NodeRun) itemRun {
	out := itemRun{Answer: nr.Answer, Prompt: systemPrompt(r.Agent, nr.Artifacts), Criteria: r.finalCriteria(sess, nodeID)}
	out.Cost.Activity = sess.NodeActivity(nodeID, r.Agent)
	if n := r.nodeRow(ctx, chatID, nodeID); n != nil {
		out.TraceID, out.Passed = n.TraceID, n.JudgePassed
		out.Cost.InputTokens, out.Cost.OutputTokens, out.Cost.CachedTokens = int64(n.PromptTokens), int64(n.CompletionTokens), int64(n.CachedTokens)
		out.Cost.JudgeRounds, out.Cost.WallTime = int(n.JudgeRounds), time.Duration(n.DurationMs)*time.Millisecond
	}
	return out
}

// systemPrompt picks agent's own system prompt out of everything the round resolved.
func systemPrompt(agent string, refs []ledger.ArtifactRef) ledger.ArtifactRef {
	for _, a := range refs {
		if a.Name == "system/"+agent {
			return a
		}
	}
	return ledger.ArtifactRef{}
}

// finalCriteria is nodeID's last judged round, the verdict the gate delivered on; earlier rounds were revised
// away. A re-judge within a round reuses its label, so the later score per criterion wins.
func (r *LiveItemRunner) finalCriteria(sess *bundle.Session, nodeID string) []criterionScore {
	round, latest := "", map[string]bundle.EvalScore{}
	for _, sc := range sess.EvaluationResults() {
		if sc.Node != nodeID {
			continue
		}
		if sc.Round != round {
			round, latest = sc.Round, map[string]bundle.EvalScore{}
		}
		latest[sc.Criterion] = sc
	}
	out := make([]criterionScore, 0, len(latest))
	for name, sc := range latest {
		out = append(out, criterionScore{Name: name, Score: r.Rubric.Score(name, sc.Score), Explanation: sc.Explanation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeRow reads nodeID's dag_nodes row off chatID's latest plan - nil (never an error) when the
// plan/node isn't there yet, so a run still reports rather than failing.
func (r *LiveItemRunner) nodeRow(ctx context.Context, chatID, nodeID string) *store.DagNode {
	if r.Store == nil || nodeID == "" {
		return nil
	}
	plan, err := r.Store.GetLatestDagPlan(ctx, chatID)
	if err != nil || plan == nil {
		return nil
	}
	node, err := r.Store.GetDagNode(ctx, plan.ID, nodeID)
	if err != nil {
		return nil
	}
	return node
}

// sessionFromBundleBytes writes body to a temp file and loads it via bundle.Load, mirroring
// internal/cli/eval.go's fetchEvalScores.
func sessionFromBundleBytes(body []byte) (*bundle.Session, error) {
	f, err := os.CreateTemp("", "quack-experiment-*.zip")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return bundle.Load(f.Name())
}

// FormatExperimentSummary renders the experiment command's per-item table.
func FormatExperimentSummary(results []ExperimentResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-40s %-34s %-20s %-10s %s\n", "ITEM ID", "TRACE ID", "PROMPT", "DURATION", "ERROR")
	for _, r := range results {
		fmt.Fprintf(&b, "%-40s %-34s %-20s %-10s %s\n", r.ItemID, r.TraceID, r.Prompt, r.Duration.Round(time.Millisecond), r.Error)
	}
	fmt.Fprintf(&b, "%d item(s) run\n", len(results))
	return b.String()
}
