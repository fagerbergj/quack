// experiment.go: `quack experiment run`. --prompt is run-item metadata only (real pinning
// needs a Version param on internal/artifactsrc.Source.Get, owned by p2/langfuse-source);
// TraceID is a minted correlation id, not the node's real OTel trace id (see final report).
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/replay"
)

// ExperimentOpts selects the dataset/agent/prompt version for `quack experiment run`.
type ExperimentOpts struct {
	Dataset string
	Agent   string
	Prompt  string // "system/<agent>@N", metadata only - see RunExperiment's doc
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

// RunExperiment runs opts.Agent's node against every item in opts.Dataset outside any live
// GitHub event, reusing the create-chat/send-message seam `quack eval` drives a chat through.
// See the package doc comment (experiment.go's header) for two known scope limits.
func RunExperiment(ctx context.Context, out, errOut io.Writer, base string, lf *langfusegen.ClientWithResponses, opts ExperimentOpts) ([]ExperimentResult, error) {
	items, err := listDatasetItems(ctx, lf, opts.Dataset, opts.Limit)
	if err != nil {
		return nil, err
	}

	c := &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
	var results []ExperimentResult
	for _, item := range items {
		res, err := runExperimentItem(ctx, out, c, lf, item, opts)
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

func runExperimentItem(ctx context.Context, out io.Writer, c *Client, lf *langfusegen.ClientWithResponses, item langfusegen.DatasetItem, opts ExperimentOpts) (ExperimentResult, error) {
	task, err := itemTask(item)
	if err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: %w", item.Id, err)
	}
	traceID, err := newTraceID()
	if err != nil {
		return ExperimentResult{}, err
	}

	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: create chat: %w", item.Id, err)
	}
	start := time.Now()
	st := newStreamState()
	onEvent := func(ev SSEEvent) error { st.handle(ev, nil); return nil }
	if err := c.SendMessage(ctx, chatID, task, onEvent); err != nil {
		return ExperimentResult{}, fmt.Errorf("experiment run: item %s: %w", item.Id, err)
	}
	res := st.result(chatID)
	duration := time.Since(start)
	if res.Status == StatusFailed {
		return ExperimentResult{ItemID: item.Id, TraceID: traceID, Prompt: opts.Prompt, Duration: duration, Error: res.Error}, nil
	}

	answer := res.Answer
	if a, ok := agentAnswer(ctx, c, chatID, opts.Agent); ok {
		answer = a
	}
	if err := recordRunItem(ctx, lf, opts, item.Id, chatID, traceID, answer); err != nil {
		return ExperimentResult{}, err
	}
	return ExperimentResult{ItemID: item.Id, TraceID: traceID, Prompt: opts.Prompt, Duration: duration}, nil
}

// agentAnswer re-fetches chatID's own recording and extracts opts.Agent's node answer
// (replay.Session.NodeRuns) instead of the whole chat's final answer - best-effort, since the
// chat's own top-level answer is a fine fallback when the fresh recording can't be read back.
func agentAnswer(ctx context.Context, c *Client, chatID, agent string) (string, bool) {
	body, err := c.FetchRecording(ctx, chatID)
	if err != nil {
		return "", false
	}
	sess, err := sessionFromBundleBytes(body)
	if err != nil {
		return "", false
	}
	for _, run := range sess.NodeRuns(map[string]bool{agent: true}) {
		if run.Answer != "" {
			return run.Answer, true
		}
	}
	return "", false
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

func recordRunItem(ctx context.Context, lf *langfusegen.ClientWithResponses, opts ExperimentOpts, itemID, chatID, traceID, answer string) error {
	req := langfusegen.CreateDatasetRunItemRequest{
		DatasetItemId: itemID,
		RunName:       opts.RunName,
		TraceId:       &traceID,
		Metadata:      map[string]string{"prompt": opts.Prompt, "agent": opts.Agent, "chat_id": chatID, "answer": answer},
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

// newTraceID mints a random 16-byte OTel-shaped trace id for run-item correlation - see
// RunExperiment's doc for why this isn't the model call's real trace id.
func newTraceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
