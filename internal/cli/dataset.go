package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/store"
)

// datasetAgents: which agents' node runs `dataset export` turns into items -
// the reviewer/synthesizer roles the issue's evaluators score (#1424).
var datasetAgents = map[string]bool{"code-reviewer": true, "synthesizer": true}

// ExportOpts selects which chats/nodes `dataset export` reads.
type ExportOpts struct {
	ChatID  string
	Repo    string
	Since   time.Time
	Dataset string
	Limit   int
}

// ExportItem is one exported dataset item, reported back for the summary table.
type ExportItem struct {
	ItemID     string
	ChatID     string
	NodeID     string
	Agent      string
	NoExpected bool // true when the item was exported with no expectedOutput (chat unmerged or no answer)
}

// datasetItemInput is a dataset item's `input` field: the node's task plus
// DiffRef, a link to the reviewed diff rather than its content (issue #1424).
type datasetItemInput struct {
	Task    string `json:"task"`
	DiffRef string `json:"diff_ref,omitempty"`
}

type datasetItemMetadata struct {
	Repo            string `json:"repo,omitempty"`
	Agent           string `json:"agent"`
	ChatID          string `json:"chat_id"`
	NodeID          string `json:"node_id"`
	PromptArtifact  string `json:"prompt_artifact,omitempty"`
	PromptSource    string `json:"prompt_source,omitempty"`
	PromptVersionID string `json:"prompt_version_id,omitempty"`
	QuackVersion    string `json:"quack_version,omitempty"`
	// Artifacts/Plugins: every artifact/plugin the answer round resolved -
	// the full audit trail PromptSource/PromptVersionID summarize to one entry.
	Artifacts []ledger.ArtifactRef `json:"artifacts,omitempty"`
	Plugins   []ledger.PluginRef   `json:"plugins,omitempty"`
}

// RunDatasetExport upserts one Langfuse dataset item per gated node run, keyed on (chat, node).
// excludedBySince is true only when --chat plus --since excluded the named chat entirely.
func RunDatasetExport(ctx context.Context, ls ledger.LedgerStore, st *store.Store, lf *langfuse.Client, opts ExportOpts) (items []ExportItem, excludedBySince bool, err error) {
	chats, excludedBySince, err := exportChats(ctx, st, opts)
	if err != nil {
		return nil, false, err
	}

	ensured := false
	for _, chat := range chats {
		sess, sessErr := bundle.FromStore(ctx, ls, chat.ID)
		if sessErr != nil {
			if errors.Is(sessErr, ledger.ErrNoRecording) {
				continue
			}
			return items, excludedBySince, fmt.Errorf("dataset export: chat %q: %w", chat.ID, sessErr)
		}
		runs := sess.NodeRuns(datasetAgents)
		for _, key := range sortedStreamKeys(runs) {
			if opts.Limit > 0 && len(items) >= opts.Limit {
				return items, excludedBySince, nil
			}
			if !ensured {
				if err := ensureDataset(ctx, lf, opts.Dataset); err != nil {
					return items, excludedBySince, err
				}
				ensured = true
			}
			item, err := exportItem(ctx, lf, opts.Dataset, chat, key, runs[key])
			if err != nil {
				return items, excludedBySince, err
			}
			items = append(items, item)
		}
	}
	return items, excludedBySince, nil
}

// sortedStreamKeys orders NodeRuns' keys by answer recency (run.At, newest
// first, ties broken by key string), so --limit favors the most recent run.
func sortedStreamKeys(runs map[bundle.StreamKey]bundle.NodeRun) []bundle.StreamKey {
	keys := make([]bundle.StreamKey, 0, len(runs))
	for k := range runs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ai, aj := runs[keys[i]].At, runs[keys[j]].At
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return keys[i].String() < keys[j].String()
	})
	return keys
}

// exportChats resolves opts to the chats to scan: one (--chat) or every chat in --repo
// updated at or after --since, paged through Store.ListChats until the page turns too old.
func exportChats(ctx context.Context, st *store.Store, opts ExportOpts) (chats []store.Chat, excludedBySince bool, err error) {
	if opts.ChatID != "" {
		return exportSingleChat(ctx, st, opts)
	}
	var out []store.Chat
	token := ""
	for {
		page, next, err := st.ListChats(ctx, store.ChatsPageMaxLimit, token, store.ChatsScope{Active: true, Archived: true})
		if err != nil {
			return nil, false, fmt.Errorf("dataset export: list chats: %w", err)
		}
		for _, c := range page {
			if repo, _, _ := c.GitHub(); opts.Repo != "" && repo != opts.Repo {
				continue
			}
			if !opts.Since.IsZero() && c.UpdatedAt.Before(opts.Since) {
				continue
			}
			out = append(out, c)
		}
		if next == "" || len(page) == 0 {
			return out, false, nil
		}
		// ListChats is updated_at-desc; once a whole page is older than --since, nothing further qualifies.
		if !opts.Since.IsZero() && page[len(page)-1].UpdatedAt.Before(opts.Since) {
			return out, false, nil
		}
		token = next
	}
}

// exportSingleChat resolves --chat; excludedBySince tells a --since exclusion apart from zero runs.
func exportSingleChat(ctx context.Context, st *store.Store, opts ExportOpts) (chats []store.Chat, excludedBySince bool, err error) {
	c, err := st.GetChat(ctx, opts.ChatID)
	if err != nil {
		return nil, false, fmt.Errorf("dataset export: get chat %q: %w", opts.ChatID, err)
	}
	if c == nil {
		return nil, false, fmt.Errorf("dataset export: chat %q not found", opts.ChatID)
	}
	if !opts.Since.IsZero() && c.UpdatedAt.Before(opts.Since) {
		return nil, true, nil
	}
	return []store.Chat{*c}, false, nil
}

// exportItemID hashes the dataset name in too: Langfuse item ids are project-scoped and
// can't be reused across datasets.
func exportItemID(dataset, chatID, nodeID string) string {
	sum := sha256.Sum256([]byte(dataset + "/" + chatID + "/" + nodeID))
	return "quack-" + hex.EncodeToString(sum[:])[:32]
}

func exportItem(ctx context.Context, lf *langfuse.Client, dataset string, chat store.Chat, key bundle.StreamKey, run bundle.NodeRun) (ExportItem, error) {
	repo, href, state := chat.GitHub()
	input := datasetItemInput{Task: run.Task, DiffRef: href}
	var expected any
	if state == string(extsdk.SubjectMerged) && run.Answer != "" {
		expected = run.Answer
	}
	meta := datasetItemMetadata{
		Repo: repo, Agent: key.Agent, ChatID: chat.ID, NodeID: key.Node,
		PromptArtifact: "system/" + key.Agent, PromptSource: run.PromptSource,
		PromptVersionID: run.PromptVersionID, QuackVersion: run.QuackVersion,
		Artifacts: run.Artifacts, Plugins: run.Plugins,
	}
	id := exportItemID(dataset, chat.ID, key.Node)
	req := langfuse.CreateDatasetItemRequest{
		DatasetName: dataset, ID: id, Input: input, ExpectedOutput: expected, Metadata: meta,
	}
	if err := lf.CreateDatasetItem(ctx, req); err != nil {
		return ExportItem{}, fmt.Errorf("dataset export: create item for chat %q node %q: %w", chat.ID, key.Node, err)
	}
	return ExportItem{ItemID: id, ChatID: chat.ID, NodeID: key.Node, Agent: key.Agent, NoExpected: expected == nil}, nil
}

// ensureDataset creates the named Langfuse dataset if it doesn't already exist -
// called only once at least one item is ready to export, never speculatively.
func ensureDataset(ctx context.Context, lf *langfuse.Client, name string) error {
	exists, err := lf.DatasetExists(ctx, name)
	if err != nil {
		return fmt.Errorf("dataset export: get dataset %q: %w", name, err)
	}
	if exists {
		return nil
	}
	if err := lf.CreateDataset(ctx, name); err != nil {
		return fmt.Errorf("dataset export: create dataset %q: %w", name, err)
	}
	return nil
}

// FormatExportSummary renders the export command's item table.
func FormatExportSummary(items []ExportItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-40s %-24s %-16s %s\n", "ITEM ID", "CHAT", "NODE", "AGENT")
	noExpected := 0
	for _, it := range items {
		fmt.Fprintf(&b, "%-40s %-24s %-16s %s\n", it.ItemID, it.ChatID, it.NodeID, it.Agent)
		if it.NoExpected {
			noExpected++
		}
	}
	fmt.Fprintf(&b, "%d item(s) exported (%d without expected output)\n", len(items), noExpected)
	return b.String()
}
