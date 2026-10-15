package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/artifact"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// datasetAgents: which agents' node runs `dataset export` turns into items - the
// high-traffic prompts worth measuring a change against.
var datasetAgents = map[string]bool{"code-reviewer": true, "synthesizer": true, "web-researcher": true, "trend-scout": true}

// ExportOpts selects which chats/nodes `dataset export` reads.
type ExportOpts struct {
	ChatID  string
	Repo    string
	Since   time.Time
	Dataset string
	Limit   int
	// Agent narrows datasetAgents to one, so a dataset holds only the agent its experiment re-runs.
	Agent string
	// Artifacts reads the input artifacts each task's <artifacts> manifest lists; nil exports none.
	Artifacts artifact.Service
}

// ExportItem is one exported dataset item, reported back for the summary table.
type ExportItem struct {
	ItemID     string
	ChatID     string
	NodeID     string
	Agent      string
	NoExpected bool // true when the item was exported with no expectedOutput (chat unmerged or no answer)
	Clone      bool // true when the item carries a pinned clone (input.setup) for its replay
}

// datasetItemInput is a dataset item's `input` field: the node's task plus
// DiffRef, a link to the reviewed diff rather than its content (issue #1424).
type datasetItemInput struct {
	Task    string `json:"task"`
	DiffRef string `json:"diff_ref,omitempty"`
	// Setup is the clone a replay checks out first: a reviewed PR's head, pinned to the commit the run saw.
	Setup *extsdk.Setup `json:"setup,omitempty"`
	// Artifacts: each bytes:<name> the task's manifest lists, by name, so the replay's fresh chat can serve it.
	Artifacts map[string]string `json:"artifacts,omitempty"`
	// SkippedArtifacts: listed artifacts left out whole because the item would pass itemBudget.
	SkippedArtifacts []string `json:"skipped_artifacts,omitempty"`
}

// itemBudget caps an item's serialized body under Langfuse's 4.5 MB request limit (a 5 MB POST gets a 413).
const itemBudget = 4 << 20

type datasetItemMetadata struct {
	Repo            string `json:"repo,omitempty"`
	Agent           string `json:"agent"`
	ChatID          string `json:"chat_id"`
	NodeID          string `json:"node_id"`
	TurnID          string `json:"turn_id,omitempty"`
	PromptArtifact  string `json:"prompt_artifact,omitempty"`
	PromptSource    string `json:"prompt_source,omitempty"`
	PromptVersionID string `json:"prompt_version_id,omitempty"`
	QuackVersion    string `json:"quack_version,omitempty"`
	// Artifacts/Plugins: every artifact/plugin the answer round resolved -
	// the full audit trail PromptSource/PromptVersionID summarize to one entry.
	Artifacts []ledger.ArtifactRef `json:"artifacts,omitempty"`
	Plugins   []ledger.PluginRef   `json:"plugins,omitempty"`
}

// exportSource is where `dataset export` reads chats, recordings, replay setups and input artifacts.
type exportSource interface {
	chats(ctx context.Context, opts ExportOpts) (chats []store.Chat, excludedBySince bool, err error)
	session(ctx context.Context, chatID string) (*bundle.Session, error) // ledger.ErrNoRecording when none
	setup(ctx context.Context, chat store.Chat, key bundle.StreamKey) (*extsdk.Setup, error)
	artifact(ctx context.Context, chat store.Chat, name string, rev int) (data []byte, ok bool, err error)
}

// RunDatasetExport upserts one Langfuse dataset item per gated node run, keyed on (chat, node, turn).
// excludedBySince is true only when --chat plus --since excluded the named chat entirely.
func RunDatasetExport(ctx context.Context, ls ledger.LedgerStore, st *store.Store, lf *langfuse.Client, opts ExportOpts) (items []ExportItem, excludedBySince bool, err error) {
	return runDatasetExport(ctx, localSource{ls: ls, st: st, svc: opts.Artifacts}, lf, opts)
}

// RunRemoteDatasetExport is RunDatasetExport reading from a quack server's API instead of the local stores.
func RunRemoteDatasetExport(ctx context.Context, c *Client, lf *langfuse.Client, opts ExportOpts) (items []ExportItem, excludedBySince bool, err error) {
	return runDatasetExport(ctx, remoteSource{c: c}, lf, opts)
}

func runDatasetExport(ctx context.Context, src exportSource, lf *langfuse.Client, opts ExportOpts) (items []ExportItem, excludedBySince bool, err error) {
	agents := datasetAgents
	if opts.Agent != "" {
		if !datasetAgents[opts.Agent] {
			return nil, false, fmt.Errorf("dataset export: --agent %s: only code-reviewer, synthesizer, web-researcher and trend-scout runs are exported", opts.Agent)
		}
		agents = map[string]bool{opts.Agent: true}
	}
	chats, excludedBySince, err := src.chats(ctx, opts)
	if err != nil {
		return nil, false, err
	}

	ensured := false
	for _, chat := range chats {
		sess, sessErr := src.session(ctx, chat.ID)
		if sessErr != nil {
			if errors.Is(sessErr, ledger.ErrNoRecording) {
				continue
			}
			return items, excludedBySince, fmt.Errorf("dataset export: chat %q: %w", chat.ID, sessErr)
		}
		runs := sess.NodeRuns(agents)
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
			setup, err := src.setup(ctx, chat, key)
			if err != nil {
				return items, excludedBySince, fmt.Errorf("dataset export: chat %q: %w", chat.ID, err)
			}
			inputs, err := inputArtifacts(ctx, src, chat, runs[key].Task)
			if err != nil {
				return items, excludedBySince, fmt.Errorf("dataset export: chat %q: %w", chat.ID, err)
			}
			item, err := exportItem(ctx, lf, opts.Dataset, chat, key, runs[key], setup, inputs)
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
			if exportable(c, opts) {
				out = append(out, c)
			}
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
	return oneChat(*c, opts)
}

// oneChat is --chat's result: c, or excludedBySince when --since rules it out entirely.
func oneChat(c store.Chat, opts ExportOpts) (chats []store.Chat, excludedBySince bool, err error) {
	if !opts.Since.IsZero() && c.UpdatedAt.Before(opts.Since) {
		return nil, true, nil
	}
	return []store.Chat{c}, false, nil
}

// exportable is c within --repo and --since. Experiment replays re-run dataset items, so exporting
// one would feed a dataset its own outputs.
func exportable(c store.Chat, opts ExportOpts) bool {
	repo, _, _ := c.GitHub()
	return !strings.HasPrefix(c.ID, "ext:"+experimentExtension+":") &&
		(opts.Repo == "" || repo == opts.Repo) &&
		(opts.Since.IsZero() || !c.UpdatedAt.Before(opts.Since))
}

// exportItemID hashes the dataset name in too: Langfuse item ids are project-scoped and
// can't be reused across datasets.
func exportItemID(dataset, chatID, nodeID, turnID string) string {
	sum := sha256.Sum256([]byte(dataset + "/" + chatID + "/" + nodeID + "/" + turnID))
	return "quack-" + hex.EncodeToString(sum[:])[:32]
}

// replaySetup is the PR head clone key's run reviewed: the chat's dispatched setup, pinned to the head its
// node started on (its revisions' lineage). nil for a chat with no PR head or a run that recorded none.
func replaySetup(ctx context.Context, ls ledger.LedgerStore, chat store.Chat, key bundle.StreamKey) (*extsdk.Setup, error) {
	var origin struct {
		Setup *extsdk.Setup `json:"quackSetup"`
	}
	if json.Unmarshal([]byte(chat.Origin), &origin) != nil || origin.Setup == nil || origin.Setup.ExistingHeadRef == "" {
		return nil, nil
	}
	revs, err := ledger.ReadByKinds(ctx, ls, chat.ID, 0, []string{ledger.KindArtifactRevision})
	if err != nil {
		return nil, err
	}
	heads := make([]headRev, 0, len(revs))
	for _, e := range revs {
		var p struct {
			Lineage recordstore.Lineage `json:"lineage"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			heads = append(heads, headRev{node: e.NodeID, turn: e.TurnID, sha: p.Lineage.HeadSHA})
		}
	}
	return pinnedSetup(*origin.Setup, heads, key), nil
}

// headRev is one artifact revision's authoring node and turn, and the head commit it saw.
type headRev struct{ node, turn, sha string }

// pinnedSetup is base pinned to the head of key's first revision (in save order) that recorded one; nil if none did.
func pinnedSetup(base extsdk.Setup, revs []headRev, key bundle.StreamKey) *extsdk.Setup {
	for _, r := range revs {
		if r.node == key.Node && r.turn == key.Turn && r.sha != "" {
			base.HeadSHA = r.sha
			return &base
		}
	}
	return nil
}

// manifestEntryRe matches one input artifact in the <artifacts> manifest extensions/github renders into a task.
var manifestEntryRe = regexp.MustCompile(`<artifact id="bytes:([^"]+)" revision="(\d+)"`)

// inputArtifacts reads each input artifact task's manifest lists, at its listed revision, from chat; one that
// no longer exists is left out.
func inputArtifacts(ctx context.Context, src exportSource, chat store.Chat, task string) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range manifestEntryRe.FindAllStringSubmatch(task, -1) {
		rev, _ := strconv.Atoi(m[2]) // \d+ always parses
		data, ok, err := src.artifact(ctx, chat, m[1], rev)
		if err != nil {
			return nil, err
		}
		if ok {
			out[m[1]] = string(data)
		}
	}
	return out, nil
}

// localSource reads the local config's stores directly.
type localSource struct {
	ls  ledger.LedgerStore
	st  *store.Store
	svc artifact.Service // nil exports no input artifacts
}

func (s localSource) chats(ctx context.Context, opts ExportOpts) ([]store.Chat, bool, error) {
	return exportChats(ctx, s.st, opts)
}

func (s localSource) session(ctx context.Context, chatID string) (*bundle.Session, error) {
	return bundle.FromStore(ctx, s.ls, chatID)
}

func (s localSource) setup(ctx context.Context, chat store.Chat, key bundle.StreamKey) (*extsdk.Setup, error) {
	return replaySetup(ctx, s.ls, chat, key)
}

func (s localSource) artifact(ctx context.Context, chat store.Chat, name string, rev int) ([]byte, bool, error) {
	if s.svc == nil {
		return nil, false, nil
	}
	return recordstore.New(s.svc, artifactref.AppName, store.SessionUserFor(chat), chat.ID).LoadVersion(ctx, "bytes:"+name, rev)
}

// remoteSource reads a quack server's API: the chat list, each chat's recording bundle, and its artifacts.
type remoteSource struct{ c *Client }

func (s remoteSource) chats(ctx context.Context, opts ExportOpts) ([]store.Chat, bool, error) {
	if opts.ChatID != "" {
		d, err := s.c.GetChat(ctx, opts.ChatID)
		if errors.Is(err, ErrNotFound) {
			return nil, false, fmt.Errorf("dataset export: chat %q not found", opts.ChatID)
		}
		if err != nil {
			return nil, false, fmt.Errorf("dataset export: get chat %q: %w", opts.ChatID, err)
		}
		return oneChat(remoteChat(d.Id, d.UpdatedAt, d.GithubRepo, d.GithubUrl, (*string)(d.GithubState)), opts)
	}
	all, err := s.c.ListChats(ctx, []string{"active", "archived"})
	if err != nil {
		return nil, false, fmt.Errorf("dataset export: list chats: %w", err)
	}
	var out []store.Chat
	for _, cs := range all {
		if c := remoteChat(cs.Id, cs.UpdatedAt, cs.GithubRepo, cs.GithubUrl, (*string)(cs.GithubState)); exportable(c, opts) {
			out = append(out, c)
		}
	}
	return out, false, nil
}

// remoteChat is an API chat as the row the export reads. The server fills github_* from its own
// Chat.GitHub(), so the legacy columns GitHub() falls back to reproduce it exactly.
func remoteChat(id string, updatedAt time.Time, repo, href, state *string) store.Chat {
	return store.Chat{ID: id, UpdatedAt: updatedAt, GithubRepo: deref(repo), GithubURL: deref(href), GithubState: deref(state)}
}

func (s remoteSource) session(ctx context.Context, chatID string) (*bundle.Session, error) {
	body, err := s.c.FetchRecording(ctx, chatID)
	if errors.Is(err, ErrNotFound) {
		return nil, ledger.ErrNoRecording
	}
	if err != nil {
		return nil, err
	}
	return sessionFromBundleBytes(body)
}

// setup is replaySetup over the API: ChatDetail.setup for the clone, artifact revision lineage for its head.
func (s remoteSource) setup(ctx context.Context, chat store.Chat, key bundle.StreamKey) (*extsdk.Setup, error) {
	d, err := s.c.GetChat(ctx, chat.ID)
	if err != nil || d.Setup == nil || deref(d.Setup.ExistingHeadRef) == "" {
		return nil, err
	}
	arts, err := s.c.ListChatArtifacts(ctx, chat.ID)
	if err != nil {
		return nil, err
	}
	var revs []schema.ArtifactRevisionInfo
	for _, a := range arts {
		revs = append(revs, a.Revisions...)
	}
	// The ledger reads revisions in save order; created_at is the API's closest stand-in.
	slices.SortStableFunc(revs, func(a, b schema.ArtifactRevisionInfo) int { return deref(a.CreatedAt).Compare(deref(b.CreatedAt)) })
	heads := make([]headRev, len(revs))
	for i, r := range revs {
		l := deref(r.Lineage)
		heads[i] = headRev{node: deref(l.NodeId), turn: deref(r.TurnId), sha: deref(l.HeadSha)}
	}
	base := extsdk.Setup{Repo: deref(d.Setup.Repo), BaseRef: deref(d.Setup.BaseRef), WorkBranch: deref(d.Setup.WorkBranch), ExistingHeadRef: deref(d.Setup.ExistingHeadRef)}
	return pinnedSetup(base, heads, key), nil
}

func (s remoteSource) artifact(ctx context.Context, chat store.Chat, name string, rev int) ([]byte, bool, error) {
	data, err := s.c.FetchArtifact(ctx, chat.ID, "bytes:"+name, rev)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	return data, err == nil, err
}

// fitArtifacts inlines inputs smallest-first while req stays within itemBudget. One that doesn't fit is left out
// whole and named in SkippedArtifacts: a truncated JSON array misleads a replay more than a missing one.
func fitArtifacts(req *langfuse.CreateDatasetItemRequest, input *datasetItemInput, inputs map[string]string, chatID string) {
	input.Artifacts = map[string]string{}
	bySize := func(a, b string) int {
		return cmp.Or(cmp.Compare(len(inputs[a]), len(inputs[b])), strings.Compare(a, b))
	}
	for _, name := range slices.SortedFunc(maps.Keys(inputs), bySize) {
		input.Artifacts[name] = inputs[name]
		if b, err := json.Marshal(req); err == nil && len(b) <= itemBudget {
			continue
		}
		delete(input.Artifacts, name)
		input.SkippedArtifacts = append(input.SkippedArtifacts, name)
		slog.Warn("dataset export: input artifact left out, the item would pass its size budget", "component", "dataset",
			"chat", chatID, "artifact", name, "bytes", len(inputs[name]), "budget", itemBudget)
	}
}

func exportItem(ctx context.Context, lf *langfuse.Client, dataset string, chat store.Chat, key bundle.StreamKey, run bundle.NodeRun, setup *extsdk.Setup, inputs map[string]string) (ExportItem, error) {
	repo, href, state := chat.GitHub()
	input := datasetItemInput{Task: run.Task, DiffRef: href, Setup: setup}
	var expected any
	if state == string(extsdk.SubjectMerged) && run.Answer != "" {
		expected = run.Answer
	}
	meta := datasetItemMetadata{
		Repo: repo, Agent: key.Agent, ChatID: chat.ID, NodeID: key.Node, TurnID: key.Turn,
		PromptArtifact: "system/" + key.Agent, PromptSource: run.PromptSource,
		PromptVersionID: run.PromptVersionID, QuackVersion: run.QuackVersion,
		Artifacts: run.Artifacts, Plugins: run.Plugins,
	}
	id := exportItemID(dataset, chat.ID, key.Node, key.Turn)
	req := langfuse.CreateDatasetItemRequest{
		DatasetName: dataset, ID: id, Input: &input, ExpectedOutput: expected, Metadata: meta,
	}
	fitArtifacts(&req, &input, inputs, chat.ID)
	if err := lf.CreateDatasetItem(ctx, req); err != nil {
		return ExportItem{}, fmt.Errorf("dataset export: create item for chat %q node %q: %w", chat.ID, key.Node, err)
	}
	return ExportItem{ItemID: id, ChatID: chat.ID, NodeID: key.Node, Agent: key.Agent, NoExpected: expected == nil, Clone: setup != nil}, nil
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
	noExpected, clones := 0, 0
	for _, it := range items {
		fmt.Fprintf(&b, "%-40s %-24s %-16s %s\n", it.ItemID, it.ChatID, it.NodeID, it.Agent)
		if it.NoExpected {
			noExpected++
		}
		if it.Clone {
			clones++
		}
	}
	fmt.Fprintf(&b, "%d item(s) exported (%d without expected output, %d with a replay clone)\n", len(items), noExpected, clones)
	return b.String()
}
