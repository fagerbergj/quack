package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

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
}

// RunDatasetExport reads gated code-reviewer/synthesizer node runs out of the ledger and
// upserts one Langfuse dataset item per run, keyed deterministically on (chat, node) so a
// re-export updates in place instead of duplicating (issue #1424).
func RunDatasetExport(ctx context.Context, ls ledger.LedgerStore, st *store.Store, lf *langfusegen.ClientWithResponses, opts ExportOpts) ([]ExportItem, error) {
	chats, err := exportChats(ctx, st, opts)
	if err != nil {
		return nil, err
	}

	var items []ExportItem
	ensured := false
	for _, chat := range chats {
		sess, err := replay.FromStore(ctx, ls, chat.ID)
		if err != nil {
			if errors.Is(err, ledger.ErrNoRecording) {
				continue
			}
			return items, fmt.Errorf("dataset export: chat %q: %w", chat.ID, err)
		}
		runs := sess.NodeRuns(datasetAgents)
		for _, key := range sortedStreamKeys(runs) {
			if opts.Limit > 0 && len(items) >= opts.Limit {
				return items, nil
			}
			if !ensured {
				if err := ensureDataset(ctx, lf, opts.Dataset); err != nil {
					return items, err
				}
				ensured = true
			}
			item, err := exportItem(ctx, lf, opts.Dataset, chat, key, runs[key])
			if err != nil {
				return items, err
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// sortedStreamKeys orders NodeRuns' keys deterministically (its map iteration
// order isn't), so a re-export or a --limit subset always picks the same runs.
func sortedStreamKeys(runs map[replay.StreamKey]replay.NodeRun) []replay.StreamKey {
	keys := make([]replay.StreamKey, 0, len(runs))
	for k := range runs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// exportChats resolves opts to the chats to scan: one (--chat) or every chat in --repo
// updated at or after --since, paged through Store.ListChats until the page turns too old.
func exportChats(ctx context.Context, st *store.Store, opts ExportOpts) ([]store.Chat, error) {
	if opts.ChatID != "" {
		c, err := st.GetChat(ctx, opts.ChatID)
		if err != nil {
			return nil, fmt.Errorf("dataset export: get chat %q: %w", opts.ChatID, err)
		}
		if c == nil {
			return nil, fmt.Errorf("dataset export: chat %q not found", opts.ChatID)
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
			if opts.Repo != "" && chatRepo(c) != opts.Repo {
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

// exportItemID derives a stable, ≤255-char Langfuse dataset item id from
// (dataset, chat, node): item ids are project-scoped and cannot be reused
// across datasets (openapi.yml:11249), so the dataset name must be in the hash.
func exportItemID(dataset, chatID, nodeID string) string {
	sum := sha256.Sum256([]byte(dataset + "/" + chatID + "/" + nodeID))
	return "quack-" + hex.EncodeToString(sum[:])[:32]
}

// chatOriginDecoded unmarshals Chat.Origin (an extension-stamped sdk.ChatOrigin,
// see the github extension's refreshChatOrigin), reporting ok=false when absent/unset.
func chatOriginDecoded(c store.Chat) (extsdk.ChatOrigin, bool) {
	if c.Origin == "" {
		return extsdk.ChatOrigin{}, false
	}
	var o extsdk.ChatOrigin
	if err := json.Unmarshal([]byte(c.Origin), &o); err != nil {
		return extsdk.ChatOrigin{}, false
	}
	return o, true
}

// chatRepo/chatMerged/chatHref read the extension-set Origin (the only field
// production writes - SetChatOrigin), falling back to the github_repo/state/url
// columns (no production writer today, kept for older or hand-seeded rows).
func chatRepo(c store.Chat) string {
	if o, ok := chatOriginDecoded(c); ok {
		if vals := o.Labels["repo"]; len(vals) > 0 && vals[0].Value != "" {
			return vals[0].Value
		}
	}
	return c.GithubRepo
}

func chatMerged(c store.Chat) bool {
	if o, ok := chatOriginDecoded(c); ok {
		return o.State == extsdk.SubjectMerged || o.Badge == "merged"
	}
	return c.GithubState == "merged"
}

func chatHref(c store.Chat) string {
	if o, ok := chatOriginDecoded(c); ok && o.Href != "" {
		return o.Href
	}
	return c.GithubURL
}

func exportItem(ctx context.Context, lf *langfusegen.ClientWithResponses, dataset string, chat store.Chat, key replay.StreamKey, run replay.NodeRun) (ExportItem, error) {
	input := datasetItemInput{Task: run.Task, DiffRef: chatHref(chat)}
	var expected any
	if chatMerged(chat) && run.Answer != "" {
		expected = run.Answer
	}
	meta := datasetItemMetadata{
		Repo: chatRepo(chat), Agent: key.Agent, ChatID: chat.ID, NodeID: key.Node,
		PromptArtifact: "system/" + key.Agent, PromptSource: run.PromptSource,
		PromptVersionID: run.PromptVersionID, QuackVersion: run.QuackVersion,
	}
	id := exportItemID(dataset, chat.ID, key.Node)
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

// ensureDataset creates the named Langfuse dataset if it doesn't already exist -
// called only once at least one item is ready to export, never speculatively.
func ensureDataset(ctx context.Context, lf *langfusegen.ClientWithResponses, name string) error {
	get, err := lf.DatasetsGetWithResponse(ctx, name)
	if err != nil {
		return fmt.Errorf("dataset export: get dataset %q: %w", name, err)
	}
	switch get.HTTPResponse.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		// falls through to create
	default:
		return fmt.Errorf("dataset export: get dataset %q: %s", name, get.Status())
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
