// experiment.go: `quack experiment run`. --prompt pins the in-process resolver to one
// Langfuse version through langfuse.PinnedSource (cmd/quack/experiment.go wires it).
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
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
	// Concurrency caps items in flight; 0 runs every item at once.
	Concurrency int
}

// ExperimentResult is one dataset item's run, printed in the summary table.
type ExperimentResult struct {
	ItemID   string
	TraceID  string
	Prompt   string
	Duration time.Duration
	Error    string
}

// experimentItemInput mirrors dataset export's datasetItemInput - only Task, Setup and Artifacts are read back.
type experimentItemInput struct {
	Task      string            `json:"task"`
	Setup     *extsdk.Setup     `json:"setup,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
	// SkippedArtifacts: inputs the export left out for size; the run item names them so a comparison can exclude it.
	SkippedArtifacts []string `json:"skipped_artifacts,omitempty"`
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
	// WorkerModel/JudgeModel: what the node and its judge actually called, so arms on different models stay apart.
	WorkerModel, JudgeModel string
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

// itemRunner runs one item's input; it's the seam that lets RunExperiment be tested without booting serve.
type itemRunner interface {
	RunItem(ctx context.Context, in experimentItemInput) (itemRun, error)
}

// RunExperiment runs opts.Agent's node against every item in opts.Dataset outside any live
// GitHub event, via runner (see itemRunner), reporting each as a Langfuse dataset run item.
func RunExperiment(ctx context.Context, errOut io.Writer, runner itemRunner, lf *langfuse.Client, opts ExperimentOpts) ([]ExperimentResult, error) {
	items, err := listDatasetItems(ctx, lf, opts.Dataset, opts.Limit)
	if err != nil {
		return nil, err
	}

	// A slot is taken before the failed check, so a hard error starts no further item; in-flight ones still report.
	results := make([]ExperimentResult, len(items))
	slots := make(chan struct{}, max(cmp.Or(opts.Concurrency, len(items)), 1))
	var mu sync.Mutex
	var failed atomic.Bool
	var g errgroup.Group
	for i, item := range items {
		slots <- struct{}{}
		if failed.Load() {
			break
		}
		g.Go(func() error {
			defer func() { <-slots }()
			res, err := runExperimentItem(ctx, runner, lf, item, opts)
			if err != nil {
				failed.Store(true)
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			results[i] = res
			fmt.Fprintf(errOut, "item %s: trace=%s prompt=%s duration=%s\n", res.ItemID, res.TraceID, res.Prompt, res.Duration)
			return nil
		})
	}
	err = g.Wait()
	results = slices.DeleteFunc(results, func(r ExperimentResult) bool { return r.ItemID == "" }) // a zero slot never ran
	if err != nil {
		// Print what already completed - a hard error otherwise drops it from the summary table/--as-json.
		fmt.Fprint(errOut, FormatExperimentSummary(results))
		return results, err
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
	in, err := itemInput(item)
	if err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: %w", item.ID, err)
	}
	start := time.Now()
	run, err := runner.RunItem(ctx, in)
	res := ExperimentResult{ItemID: item.ID, TraceID: run.TraceID, Prompt: run.prompt(), Duration: time.Since(start)}
	switch {
	case err != nil:
		res.Error = err.Error()
	case run.TraceID == "":
		res.Error = "no trace id recorded for the node run"
	case opts.Prompt != "" && run.prompt() != opts.Prompt:
		// A pin the node never resolved (its prompt came from somewhere else) would compare the wrong version.
		res.Error = fmt.Sprintf("node ran prompt %q, not the pinned %s", run.prompt(), opts.Prompt)
	}
	if res.Error != "" {
		return res, nil
	}
	if err := recordRunItem(ctx, lf, opts, item.ID, run, in.SkippedArtifacts); err != nil {
		return ExperimentResult{}, err
	}
	return res, nil
}

func itemInput(item langfuse.DatasetItem) (experimentItemInput, error) {
	var in experimentItemInput
	b, err := json.Marshal(item.Input)
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, err
	}
	if in.Task == "" {
		return in, fmt.Errorf("item %s: input has no task field", item.ID)
	}
	return in, nil
}

// recordRunItem scores the trace before linking it, so a failed link leaves only scores on an unlinked
// trace. The prompt rides on the run (description and metadata); the answer already lives on the trace.
func recordRunItem(ctx context.Context, lf *langfuse.Client, opts ExperimentOpts, itemID string, run itemRun, skipped []string) error {
	if err := lf.Ingest(ctx, itemScores(run, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return fmt.Errorf("experiment run: item %s: score trace: %w", itemID, err)
	}
	meta := map[string]string{"agent": opts.Agent, "prompt_name": run.Prompt.Name,
		"prompt_version": run.Prompt.VersionID, "prompt_source": run.Prompt.Source,
		"worker_model": run.WorkerModel, "judge_model": run.JudgeModel}
	if len(skipped) > 0 {
		meta["skipped_artifacts"] = strings.Join(skipped, ",")
	}
	req := langfuse.CreateDatasetRunItemRequest{
		DatasetItemID: itemID, RunName: opts.RunName, TraceID: run.TraceID,
		RunDescription: fmt.Sprintf("prompt=%s agent=%s model=%s judge=%s", run.prompt(), opts.Agent, run.WorkerModel, run.JudgeModel),
		Metadata:       meta,
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
	// Cleanup removes an item's chat workspace once the item is read back, whatever its outcome; nil keeps it.
	Cleanup func(chatID string)
}

func NewLiveItemRunner(base string, st *store.Store, agent string, rubric vetting.RubricScaler) *LiveItemRunner {
	return &LiveItemRunner{Base: base, Store: st, Agent: agent, Rubric: rubric}
}

// RunItem runs Agent's node alone on in's task, in in.Setup's clone if any, through the experiment
// extension's bound workflow, then reads the node back from the chat's recording.
func (r *LiveItemRunner) RunItem(ctx context.Context, in experimentItemInput) (itemRun, error) {
	ext := liveExperiment.Load()
	if ext == nil {
		return itemRun{}, fmt.Errorf("experiment extension not running (EnableExperiment before boot)")
	}
	chatID, err := ext.run(ctx, r.Agent, in)
	if chatID != "" && r.Cleanup != nil {
		defer r.Cleanup(chatID)
	}
	if err != nil {
		return itemRun{}, err
	}
	c := &Client{BaseURL: strings.TrimRight(r.Base, "/"), HTTP: &http.Client{}}
	body, err := c.FetchRecording(ctx, chatID)
	if errors.Is(err, ErrNotFound) {
		return itemRun{}, fmt.Errorf("chat %s: no recording: the run ended before any model call, e.g. its clone or setup failed (see the log)", chatID)
	}
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
	return itemRun{}, fmt.Errorf("chat %s: agent %q produced no node run", chatID, r.Agent)
}

// experimentExtension is the in-process SDK extension `experiment run` dispatches through. Its bound
// one-node workflow runs the agent's node on the recorded task with no orchestrator or planner turn.
const experimentExtension = "experiment"

func init() { extsdk.Register(experimentExtension, newExperimentExt) }

// experimentItemTimeout surfaces a hung item; nothing else in an item's path is a wall-clock wait, since a
// run's start under load (clone, check_setup, admission) can take minutes. Items take 2-20 min.
const experimentItemTimeout = 2 * time.Hour

// liveExperiment is the booted extension; `experiment run` boots one in-process server per command.
var liveExperiment atomic.Pointer[experimentExt]

// EnableExperiment configures the in-process server `experiment run` boots: the experiment extension
// and agent's bound workflow, whose one node's task is the item's recorded task verbatim.
func EnableExperiment(cfg *config.Config, agent string) {
	if cfg.Extensions.Modules == nil {
		cfg.Extensions.Modules = map[string]yaml.Node{}
	}
	cfg.Extensions.Modules[experimentExtension] = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	cfg.Workflows = append(cfg.Workflows, config.WorkflowShape{
		Name: experimentWorkflow(agent), Trigger: "quack experiment run only", Shape: agent, Agents: []string{agent},
		Nodes: []config.WorkflowNode{{ID: agent + "-1", Agent: agent, Task: "{{ask}}"}},
	})
}

// OverrideModels rebinds agent's worker to worker and the gate judge to judge, each "<provider>/<model>"
// ("" keeps quack.yaml's), for this run only; boot prices and admits them from their models: entries.
func OverrideModels(cfg *config.Config, agent, worker, judge string) error {
	if worker != "" {
		ac := cfg.Agents[agent]
		if err := overrideModel(cfg, worker, &ac.Provider, &ac.Model); err != nil {
			return fmt.Errorf("--model %w", err)
		}
		if cfg.Agents == nil {
			cfg.Agents = map[string]config.AgentConfig{}
		}
		cfg.Agents[agent] = ac
	}
	if err := overrideModel(cfg, judge, &cfg.Gates.Judge.Provider, &cfg.Gates.Judge.Model); err != nil {
		return fmt.Errorf("--judge-model %w", err)
	}
	return nil
}

// overrideModel splits ref at its first slash, so an OpenRouter id like openai/gpt-6-luna keeps its own.
func overrideModel(cfg *config.Config, ref string, provider, model *string) error {
	if ref == "" {
		return nil
	}
	p, m, _ := strings.Cut(ref, "/")
	if _, ok := cfg.Providers[p]; !ok || m == "" {
		return fmt.Errorf("%q: want <provider>/<model> with a provider defined under providers", ref)
	}
	if mc, ok := cfg.Models[m]; !ok || mc.Provider != p {
		return fmt.Errorf("%q: model %q must be defined under models with provider: %s", ref, m, p)
	}
	*provider, *model = p, m
	return nil
}

func experimentWorkflow(agent string) string { return "experiment-" + agent }

// experimentUser owns a replay chat, so its input artifacts land where the run reads them.
const experimentUser = "experiment"

type experimentExt struct {
	host  extsdk.Host
	mu    sync.Mutex
	ended map[string]chan extsdk.RunOutcome
}

func newExperimentExt(host extsdk.Host, _ []byte) (extsdk.Extension, error) {
	return &experimentExt{host: host, ended: map[string]chan extsdk.RunOutcome{}}, nil
}

func (e *experimentExt) Tools() []tool.Tool             { return nil }
func (e *experimentExt) RegisterRoutes(_, _ chi.Router) {}

// Start publishes the booted extension; the factory also runs during config validation, so it can't.
func (e *experimentExt) Start(context.Context) error {
	liveExperiment.Store(e)
	return nil
}

// RunEnded hands a run's final outcome to the item waiting on it; a pause is not an end.
func (e *experimentExt) RunEnded(chatID string, o extsdk.RunOutcome) {
	if o.Status == extsdk.RunPaused {
		return
	}
	e.mu.Lock()
	ch := e.ended[chatID]
	delete(e.ended, chatID)
	e.mu.Unlock()
	if ch != nil {
		ch <- o
	}
}

// run dispatches in to agent's bound workflow and waits for the run to end. Delivery is denied
// outright, so a replayed reviewer never posts a review and a coder never pushes.
func (e *experimentExt) run(ctx context.Context, agent string, in experimentItemInput) (string, error) {
	localID := uuid.NewString()
	chatID := "ext:" + experimentExtension + ":" + localID
	// The task's <artifacts> manifest names these; without them every read_artifact of one is not-found.
	for name, content := range in.Artifacts {
		data := []byte(content)
		mime := "text/plain"
		if json.Valid(data) {
			mime = "application/json"
		}
		if _, _, err := e.host.WriteArtifact(chatID, experimentUser, name, mime, data); err != nil {
			return chatID, fmt.Errorf("write input artifact %s: %w", name, err) // chatID: the runner still cleans up after it
		}
	}
	ch := make(chan extsdk.RunOutcome, 1)
	e.mu.Lock()
	e.ended[chatID] = ch
	e.mu.Unlock()
	req := extsdk.DispatchRequest{
		Chat:     extsdk.ChatRef{LocalID: localID, User: experimentUser, Title: "experiment: " + agent},
		Ask:      extsdk.Ask{Message: stripRecordedEnvironment(in.Task)},
		Run:      extsdk.RunConfig{Workflow: experimentWorkflow(agent), ReadOnly: true, Setup: in.Setup, Timeout: experimentItemTimeout},
		Delivery: extsdk.DeliveryAuthority{AllowedKinds: []extsdk.DeliveryKind{}},
	}
	if err := e.host.Dispatch(ctx, req); err != nil {
		e.mu.Lock()
		delete(e.ended, chatID)
		e.mu.Unlock()
		return "", fmt.Errorf("dispatch %s: %w", agent, err)
	}
	select {
	case o := <-ch:
		if o.TimedOut {
			return chatID, fmt.Errorf("chat %s: run timed out after %s", chatID, experimentItemTimeout)
		}
		if o.Status != extsdk.RunDone {
			return chatID, fmt.Errorf("chat %s: run %s: %s", chatID, o.Status, o.Error)
		}
		return chatID, nil
	case <-ctx.Done():
		return chatID, ctx.Err()
	}
}

// stripRecordedEnvironment drops the recording's trailing <environment_context>: it names the recorded
// run's cwd, which doesn't exist here, and the replay's ACP round appends its own.
func stripRecordedEnvironment(task string) string {
	i := strings.LastIndex(task, "<environment_context>")
	if i < 0 || !strings.HasSuffix(strings.TrimSpace(task), "</environment_context>") {
		return task
	}
	return strings.TrimRight(task[:i], "\n")
}

// itemRunFor assembles nodeID's run: prompt and judge scores from the recording, tokens and timing from its
// dag_nodes row (the node_done totals); TraceID stays "" until that row exists, so the item reports rather than fails.
func (r *LiveItemRunner) itemRunFor(ctx context.Context, chatID string, sess *bundle.Session, nodeID string, nr bundle.NodeRun) itemRun {
	out := itemRun{Answer: nr.Answer, Prompt: systemPrompt(r.Agent, nr.Artifacts), Criteria: r.finalCriteria(sess, nodeID)}
	out.Cost.Activity = sess.NodeActivity(nodeID, r.Agent)
	out.JudgeModel = sess.LastModel(nodeID, ledger.JudgeAgent)
	if n := r.nodeRow(ctx, chatID, nodeID); n != nil {
		out.TraceID, out.Passed, out.WorkerModel = n.TraceID, n.JudgePassed, n.Model
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
