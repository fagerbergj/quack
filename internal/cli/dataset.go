package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
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
	ItemID string
	ChatID string
	NodeID string
	Agent  string
}

// datasetItemInput is a dataset item's `input` field: the node's task plus a
// reference to the reviewed diff, never the diff itself (issue #1424).
type datasetItemInput struct {
	Task            string   `json:"task"`
	Question        string   `json:"question"`
	UpstreamAnswers []string `json:"upstream_answers,omitempty"`
	DiffRef         string   `json:"diff_ref,omitempty"`
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
}

// RunDatasetExport reads gated code-reviewer/synthesizer node runs out of the ledger and
// upserts one Langfuse dataset item per run, keyed deterministically on (chat, node) so a
// re-export updates in place instead of duplicating (issue #1424).
func RunDatasetExport(ctx context.Context, ls ledger.LedgerStore, st *store.Store, lf *langfusegen.ClientWithResponses, opts ExportOpts) ([]ExportItem, error) {
	if err := ensureDataset(ctx, lf, opts.Dataset); err != nil {
		return nil, err
	}
	chats, err := exportChats(ctx, st, opts)
	if err != nil {
		return nil, err
	}

	var items []ExportItem
	for _, chat := range chats {
		sess, err := replay.FromStore(ctx, ls, chat.ID)
		if err != nil {
			if err == ledger.ErrNoRecording {
				continue
			}
			return items, fmt.Errorf("dataset export: chat %q: %w", chat.ID, err)
		}
		for key, run := range sess.NodeRuns(datasetAgents) {
			if opts.Limit > 0 && len(items) >= opts.Limit {
				return items, nil
			}
			item, err := exportItem(ctx, lf, opts.Dataset, chat, key, run)
			if err != nil {
				return items, err
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// exportChats resolves opts to the chats to scan: one (--chat) or every chat in --repo
// updated at or after --since, paged through Store.ListChats until the page turns too old.
func exportChats(ctx context.Context, st *store.Store, opts ExportOpts) ([]store.Chat, error) {
	if opts.ChatID != "" {
		c, err := st.GetChat(ctx, opts.ChatID)
		if err != nil {
			return nil, fmt.Errorf("dataset export: get chat %q: %w", opts.ChatID, err)
		}
		return []store.Chat{*c}, nil
	}
	var out []store.Chat
	token := ""
	for {
		page, next, err := st.ListChats(ctx, store.ChatsPageMaxLimit, token, store.ChatsScope{Active: true, Archived: true})
		if err != nil {
			return nil, fmt.Errorf("dataset export: list chats: %w", err)
		}
		for _, c := range page {
			if opts.Repo != "" && c.GithubRepo != opts.Repo {
				continue
			}
			if !opts.Since.IsZero() && c.UpdatedAt.Before(opts.Since) {
				continue
			}
			out = append(out, c)
		}
		if next == "" || len(page) == 0 {
			return out, nil
		}
		// ListChats is updated_at-desc; once a whole page is older than --since, nothing further qualifies.
		if !opts.Since.IsZero() && page[len(page)-1].UpdatedAt.Before(opts.Since) {
			return out, nil
		}
		token = next
	}
}

// exportItemID derives a stable, ≤255-char Langfuse dataset item id from (chat, node) -
// re-exporting the same node run upserts the same item instead of duplicating it.
func exportItemID(chatID, nodeID string) string {
	sum := sha256.Sum256([]byte(chatID + "/" + nodeID))
	return "quack-" + hex.EncodeToString(sum[:])[:32]
}

func exportItem(ctx context.Context, lf *langfusegen.ClientWithResponses, dataset string, chat store.Chat, key replay.StreamKey, run replay.NodeRun) (ExportItem, error) {
	input := datasetItemInput{Task: run.Task, Question: run.Task, DiffRef: chat.GithubURL}
	var expected any
	if chat.GithubState == "merged" && run.Answer != "" {
		expected = run.Answer
	}
	meta := datasetItemMetadata{
		Repo: chat.GithubRepo, Agent: key.Agent, ChatID: chat.ID, NodeID: key.Node,
		PromptArtifact: "system/" + key.Agent, PromptSource: run.PromptSource,
		PromptVersionID: run.PromptVersionID, QuackVersion: run.QuackVersion,
	}
	id := exportItemID(chat.ID, key.Node)
	req := langfusegen.CreateDatasetItemRequest{
		DatasetName: dataset, Id: &id, Input: input, ExpectedOutput: expected, Metadata: meta,
	}
	resp, err := lf.DatasetItemsCreateWithResponse(ctx, req)
	if err != nil {
		return ExportItem{}, fmt.Errorf("dataset export: create item for chat %q node %q: %w", chat.ID, key.Node, err)
	}
	if resp.JSON200 == nil {
		return ExportItem{}, fmt.Errorf("dataset export: create item for chat %q node %q: %s", chat.ID, key.Node, resp.Status())
	}
	return ExportItem{ItemID: id, ChatID: chat.ID, NodeID: key.Node, Agent: key.Agent}, nil
}

// ensureDataset creates the named Langfuse dataset if it doesn't already exist.
func ensureDataset(ctx context.Context, lf *langfusegen.ClientWithResponses, name string) error {
	get, err := lf.DatasetsGetWithResponse(ctx, name)
	if err != nil {
		return fmt.Errorf("dataset export: get dataset %q: %w", name, err)
	}
	if get.HTTPResponse.StatusCode == http.StatusOK {
		return nil
	}
	create, err := lf.DatasetsCreateWithResponse(ctx, langfusegen.CreateDatasetRequest{Name: name})
	if err != nil {
		return fmt.Errorf("dataset export: create dataset %q: %w", name, err)
	}
	if create.JSON200 == nil {
		return fmt.Errorf("dataset export: create dataset %q: %s", name, create.Status())
	}
	return nil
}

// FormatExportSummary renders the export command's item table.
func FormatExportSummary(items []ExportItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-40s %-24s %-16s %s\n", "ITEM ID", "CHAT", "NODE", "AGENT")
	for _, it := range items {
		fmt.Fprintf(&b, "%-40s %-24s %-16s %s\n", it.ItemID, it.ChatID, it.NodeID, it.Agent)
	}
	fmt.Fprintf(&b, "%d item(s) exported\n", len(items))
	return b.String()
}
