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
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/store"
)

// ExperimentOpts selects the dataset/agent/prompt version for `quack experiment run`.
type ExperimentOpts struct {
	Dataset string
	Agent   string
	Prompt  string // "system/<agent>@N", the pinned version, also recorded on the run item
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

// itemRunner executes one dataset item's task against an agent, returning its answer and the
// node's real OTel trace id. Split out of RunExperiment so the loop/summary/run-item-reporting
// can be unit tested against a stub, without booting serve.InProcessFromConfig.
type itemRunner interface {
	RunItem(ctx context.Context, task string) (answer, traceID string, err error)
}

// RunExperiment runs opts.Agent's node against every item in opts.Dataset outside any live
// GitHub event, via runner (see itemRunner), reporting each as a Langfuse dataset run item.
func RunExperiment(ctx context.Context, errOut io.Writer, runner itemRunner, lf *langfusegen.ClientWithResponses, opts ExperimentOpts) ([]ExperimentResult, error) {
	items, err := listDatasetItems(ctx, lf, opts.Dataset, opts.Limit)
	if err != nil {
		return nil, err
	}

	var results []ExperimentResult
	for _, item := range items {
		res, err := runExperimentItem(ctx, runner, lf, item, opts)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		fmt.Fprintf(errOut, "item %s: trace=%s prompt=%s duration=%s\n", res.ItemID, res.TraceID, res.Prompt, res.Duration)
	}
	return results, nil
}

func listDatasetItems(ctx context.Context, lf *langfusegen.ClientWithResponses, dataset string, limit int) ([]langfusegen.DatasetItem, error) {
	params := &langfusegen.DatasetItemsListParams{DatasetName: &dataset}
	if limit > 0 {
		params.Limit = &limit
	}
	resp, err := lf.DatasetItemsListWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("experiment run: list dataset %q items: %w", dataset, err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("experiment run: list dataset %q items: %s", dataset, resp.Status())
	}
	return resp.JSON200.Data, nil
}

func runExperimentItem(ctx context.Context, runner itemRunner, lf *langfusegen.ClientWithResponses, item langfusegen.DatasetItem, opts ExperimentOpts) (ExperimentResult, error) {
	task, err := itemTask(item)
	if err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: %w", item.Id, err)
	}
	start := time.Now()
	answer, traceID, err := runner.RunItem(ctx, task)
	duration := time.Since(start)
	if err != nil {
		return ExperimentResult{ItemID: item.Id, TraceID: traceID, Prompt: opts.Prompt, Duration: duration, Error: err.Error()}, nil
	}
	if err := recordRunItem(ctx, lf, opts, item.Id, traceID, answer); err != nil {
		return ExperimentResult{}, err
	}
	return ExperimentResult{ItemID: item.Id, TraceID: traceID, Prompt: opts.Prompt, Duration: duration}, nil
}

func itemTask(item langfusegen.DatasetItem) (string, error) {
	b, err := json.Marshal(item.Input)
	if err != nil {
		return "", err
	}
	var in experimentItemInput
	if err := json.Unmarshal(b, &in); err != nil {
		return "", err
	}
	if in.Task == "" {
		return "", fmt.Errorf("item %s: input has no task field", item.Id)
	}
	return in.Task, nil
}

func recordRunItem(ctx context.Context, lf *langfusegen.ClientWithResponses, opts ExperimentOpts, itemID, traceID, answer string) error {
	req := langfusegen.CreateDatasetRunItemRequest{
		DatasetItemId: itemID,
		RunName:       opts.RunName,
		TraceId:       &traceID,
		Metadata:      map[string]string{"prompt": opts.Prompt, "agent": opts.Agent, "answer": answer},
	}
	resp, err := lf.DatasetRunItemsCreateWithResponse(ctx, req)
	if err != nil {
		return fmt.Errorf("experiment run: item %s: create run item: %w", itemID, err)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("experiment run: item %s: create run item: %s", itemID, resp.Status())
	}
	return nil
}

// LiveItemRunner is the itemRunner `quack experiment run` actually drives: a fresh chat
// against an in-process duck (Base), with the real trace id read back from Store's DagNode
// row for Agent's node (dag_nodes.trace_id - stamped by the node-start ledger write).
type LiveItemRunner struct {
	Base  string
	Store *store.Store
	Agent string
}

func NewLiveItemRunner(base string, st *store.Store, agent string) *LiveItemRunner {
	return &LiveItemRunner{Base: base, Store: st, Agent: agent}
}

func (r *LiveItemRunner) RunItem(ctx context.Context, task string) (answer, traceID string, err error) {
	c := &Client{BaseURL: strings.TrimRight(r.Base, "/"), HTTP: &http.Client{}}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		return "", "", fmt.Errorf("create chat: %w", err)
	}
	st := newStreamState()
	onEvent := func(ev SSEEvent) error { st.handle(ev, nil); return nil }
	if err := c.SendMessage(ctx, chatID, task, onEvent); err != nil {
		return "", "", err
	}
	res := st.result(chatID)
	if res.Status == StatusFailed {
		return "", "", fmt.Errorf("%s", res.Error)
	}

	answer = res.Answer
	nodeID := ""
	if body, err := c.FetchRecording(ctx, chatID); err == nil {
		if sess, err := sessionFromBundleBytes(body); err == nil {
			for key, run := range sess.NodeRuns(map[string]bool{r.Agent: true}) {
				if run.Answer != "" {
					answer, nodeID = run.Answer, key.Node
				}
			}
		}
	}
	return answer, r.traceIDFor(ctx, chatID, nodeID), nil
}

// traceIDFor reads dag_nodes.trace_id for nodeID off chatID's latest plan - "" (never an
// error) when the plan/node/trace isn't there yet, so a run still reports rather than failing.
func (r *LiveItemRunner) traceIDFor(ctx context.Context, chatID, nodeID string) string {
	if r.Store == nil || nodeID == "" {
		return ""
	}
	plan, err := r.Store.GetLatestDagPlan(ctx, chatID)
	if err != nil || plan == nil {
		return ""
	}
	node, err := r.Store.GetDagNode(ctx, plan.ID, nodeID)
	if err != nil || node == nil {
		return ""
	}
	return node.TraceID
}

// sessionFromBundleBytes writes body to a temp file and loads it via replay.Load, mirroring
// internal/cli/eval.go's fetchEvalScores.
func sessionFromBundleBytes(body []byte) (*replay.Session, error) {
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
	return replay.Load(f.Name())
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
