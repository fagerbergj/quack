package vetting

import (
	"context"
	"iter"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/workspace"
)

// judgeSessionID groups a judge/writer run under its chat in Langfuse. Each run has its own
// throwaway session service, so reusing chatID only affects observability grouping.
func judgeSessionID(chatID, fallback string) string {
	if chatID == "" {
		return fallback
	}
	return chatID
}

// Config carries per-agent trust-gate settings for RunGatedRefine and the judge.
type Config struct {
	// NodeBaseSHA is the clone's HEAD when THIS node started: chained nodes share one clone, so the
	// reflog base would include siblings' commits. Empty falls back to the reflog base.
	NodeBaseSHA          string
	DeterministicRounds  int     // > 0 turns the gate on without a judge; logged only, never a round cap
	JudgeRounds          int     // model-judge/revise rounds
	Threshold            float64 // pass score in (0,1]
	JudgeMaxIterations   int     // cap on judge model turns per round
	JudgeContextWindow   int     // context window in tokens; 0 ⇒ default
	JudgeMaxOutputTokens int     // cap on judge/plan-judge reply tokens; <= 0 = uncapped
	JudgeThinkingLevel   string  // gates.judge.thinking_level: "", "low", "medium", "high"; "" = no ThinkingConfig sent
	Constitution         string  // global principles for judge prompt
	Rubric               string  // scoring guide; global default or per-agent override; rendered markdown for the judge prompt
	// ConstitutionArtifact/RubricArtifact/MemoryArtifact: ledger provenance; zero Name when the
	// value came from an inline gates: override or there is none to resolve.
	ConstitutionArtifact artifactsrc.Artifact
	RubricArtifact       artifactsrc.Artifact
	MemoryArtifact       artifactsrc.Artifact
	// Plugins: the plugin registry rows in scope for this node's rounds,
	// refreshed alongside Rubric/Constitution at each run's start; nil = no registry.
	Plugins []ledger.PluginRef
	// RubricSpecs: per-criterion definition/scale/bands when Rubric came from a rubric.yaml;
	// nil for a raw prose rubric override, which has no structured criteria.
	RubricSpecs map[string]criterionSpec
	// RubricFixes: declared fix text per deterministic criterion the rubric
	// names (rubricyaml.go's rubricDocFixes) - nil for a raw prose override.
	RubricFixes map[string]string

	// RubricPassMarks: each criterion's own declared pass mark - informational
	// only (buildEnvelope gates on one global Threshold); set only by judge replay.
	RubricPassMarks  map[string]float64
	RequireRetrieval bool // zero retrieval = ungrounded
	ReadOnly         bool // no delivery tools - completion is review/exploration
	IsReviewer       bool // stamped from node agent, never from task wording
	// ReviewFanout: non-nil only in a plan with >1 reviewer node. Each stages into it and the
	// last to finish delivers the merged, worst-of-verdict review exactly once.
	ReviewFanout *ReviewFanout
	Artifacts    artifact.Service // nil = read_artifact tool unavailable to this node
	// RecordReader reads a written artifact's latest body for code-owned checks;
	// nil = recordClient(cfg) (live), replay hands in its REST-backed loader.
	RecordReader PageLoader
	// RoundCoordsSink restamps a native node's already-built artifact tools with fresh round
	// coords (draft seed and every judge round), without vetting importing tools.
	RoundCoordsSink func(round int, turnID, headSHA, triggerAnnotation string)
	// Ledger: the WAL's fail-closed AppendIntent path. nil = no WAL; recordstore and the gate
	// then write projections directly.
	Ledger ledger.LedgerStore
	// Schemas: boot-collected extsdk.ArtifactSchemas registry; nil = no
	// extension declared a schema, so no recordstore write here is checked.
	Schemas *artifactschema.Registry
	// Decisions: the enabled decision points (answer.accept, research.source); nil disables them.
	Decisions *decide.Decider
	// Artifact: episodic record name written on gate pass ("" for none). IsReviewer nodes write
	// "review" regardless; this names only the extra record.
	Artifact           string
	Memory             *memory.Store // staged tradecraft on pass
	CommitMemory       bool          // task-memory participant
	MemoryRole         string        // role bucket key; empty falls back to repo then user
	DeliverPromptEvent bool          // true for A2A workers (session events)
	Checks             []string      // per-node deterministic gate commands
	DeriveChecks       bool          // derive from repo when Checks empty
	CheckCommands      []string      // prefix allowlist; empty ⇒ checks disabled
	CheckSetup         []string      // repo bootstrap commands, run once per clone (worker tree and baseline)
	NodeID             string        // workspace scope for checks/clone resolution
	AdvisorToken       string        // the node's advisor token, set by dag (never parsed from the prompt); empty = no node
	Agent              string        // observability only
	// BundleHash: the agent bundle's content hash; ledger provenance only.
	BundleHash string
	// PromptSource/PromptVersionID: where the system/<agent> artifact came from ("static" or the
	// store name) and its version; ledger provenance only.
	PromptSource    string
	PromptVersionID string
	// PromptArtifact: the resolved artifact's name, from the bundle directory, not Agent.
	PromptArtifact string
	// RefreshPrompt re-resolves the worker's system prompt at a round's start, so the prompt the
	// model sees and the version the ledger records never disagree mid-round. nil keeps the above.
	RefreshPrompt func(ctx context.Context) artifactsrc.Artifact
	// Prompts resolves the judge's own system/judge, once per judge round;
	// nil resolves the shipped file.
	Prompts *artifactsrc.Resolver
	// judgePrompt: the round's rendered system/judge, set by prepareJudge right
	// where judgeCoords are built. Zero value = runJudgeRound resolves its own.
	judgePrompt judgePrompt
	User        string // observability only; resolved from the ADK session, not caller-set
	Source      string // observability only; run origin (extension name or a fixed app value)
	Task        string // delivery check; empty = no check
	Request     string // the turn's user message, for decision states only
	// UpstreamAnswers: this node's dependency output, same as buildTask gives
	// the worker - the judge needs it too to verify upstream-sourced claims.
	UpstreamAnswers string
	Workdir         string          // for Checks; ignored when Checks empty
	ChatID          string          // per-chat workspace scope
	Workspace       *workspace.Jail // nil + non-empty Checks fails closed
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps
	// CheckTimeout overrides WorkspaceCaps.Timeout for gate check commands only
	// (they run a full test suite, not a single tool call); 0 = use WorkspaceCaps.Timeout.
	CheckTimeout   time.Duration
	Deliver        DeliverFunc         // posts staged delivery set; nil = disabled
	GitCredentials GitCredentialSource // resolves the gate-owned push credential; nil = push disabled
	// AllowedDeliveryKinds: nil = unrestricted (no trigger governs this run);
	// non-nil (including empty) restricts staged delivery to exactly these kinds.
	AllowedDeliveryKinds []string
	ExternalWorker       bool         // ACP-backed; gate supplements session ledger
	Setup                *SetupBranch // pre-cloned checkout; delivery on this branch
	ExistingPR           bool         // run pushes onto an already-open PR; stage_push offered instead of stage_pr
	// JudgeModel: the boot judge model, or whichever bound-in model prepareJudge picked for the
	// round; stamped with per-round coords like workerModel.
	JudgeModel model.LLM
	// RefreshJudgeBinding picks this round's own JudgeFactory/model/thinking_level;
	// hasReadTools is the node's own tool eligibility, kept through a rebind.
	RefreshJudgeBinding func(art artifactsrc.Artifact, hasReadTools bool) (JudgeFactory, model.LLM, string)
	// ResumedFrom: "" for a fresh node; otherwise seeds an ACP node's first-round session/load
	// id and marks node.started as a continuation.
	ResumedFrom string
	// AdmitJudge/ReleaseJudge/AdmitWorker/ReleaseWorker: swap the caller's admission
	// reservation to the judge spec for each judge call, and back. All nil without an admission ledger.
	AdmitJudge    func(ctx context.Context) bool
	ReleaseJudge  func()
	AdmitWorker   func(ctx context.Context) bool
	ReleaseWorker func()
	// TryAdmitVerify reserves one more judge-model session without waiting, for a parallel
	// verifier batch; nil means no admission ledger, so batches run in parallel unmetered.
	TryAdmitVerify func() (release func(), ok bool)
	// JudgeArtifactTools: list_artifacts/read_artifact over the chat, narrowed per
	// judge round by ForeignNodes; nil only when the run has no artifact service.
	JudgeArtifactTools []tool.Tool
	// ForeignNodes: plan nodes that are neither this node nor its upstream. Their
	// session activity and artifacts never count as this node's evidence.
	ForeignNodes []string
	// judgeEvidence: the round's CITED EVIDENCE section, set by runJudge from the verify tier;
	// judgeCheckedPages: the web_pages that tier read this round, which the judge is not shown.
	judgeEvidence     string
	judgeCheckedPages map[string]bool
}

// HasWorkspaceClone: only ExternalWorker (ACP) nodes have a repo checkout; native nodes never
// do, regardless of agent name.
func (c Config) HasWorkspaceClone() bool {
	return c.ExternalWorker && c.Workspace != nil
}

// SetupBranch mirrors dag.Plan.Setup delivery fields.
type SetupBranch struct {
	Repo       string
	WorkBranch string
}

// One item the worker staged for delivery. Branch filled by the gate.
type StagedDelivery struct {
	Kind   string // pull_request | review | comment
	Branch string
	Title  string
	Body   string
	// TitleOmitted/BodyOmitted (stage_push only): the agent omitted that field, so delivery must
	// PATCH without the key; an empty string would blank it on GitHub.
	TitleOmitted bool
	BodyOmitted  bool
	Event        string          // review verdict: approve | request_changes | comment
	Slot         string          // comment target, for Kind == "comment"
	Comments     []ReviewComment // inline findings
	Recovered    bool            // parsed from answer tail, not tool-staged
	// Takeaway/Verified/Notes (Kind "review" only): ingredients for the code_review record and
	// renderer; Body carries a rendered fallback for when no code_review artifact backs it.
	Takeaway string
	Verified []string
	Notes    []string
}

// ReviewComment: one inline, line-anchored review finding.
type ReviewComment struct {
	Path string
	Line int
	Body string
	// FindingID: the backing FindingRecord's hash id, when known, so the overview dedupes a
	// finding both written via write_finding and staged as an inline comment.
	FindingID string
	// SourceNode: the reviewer node that staged this finding in a fan-out
	// review - lineage only, never rendered into the posted body.
	SourceNode string
	// Severity: the backing finding's severity label, passed to extensions
	// because a write_finding body carries no label text.
	Severity string
}

// DeliveryContext: staged set + clone coordinates for extension delivery.
type DeliveryContext struct {
	NodeID       string
	ChatID       string // for delivery-outcome bookkeeping
	Items        []StagedDelivery
	CloneURL     string // git_clone URL; "" = nothing to deliver
	CloneDir     string // jail-resolved clone path; "" = no Workspace
	Branch       string // last branch from ledger
	IssueNumber  int    // PR number for review/comment targets
	GatePassed   bool   // false = attach caveat
	GateFeedback string // feedback for caveat when GatePassed is false
	// PushedSHA: the branch head the gate itself pushed; "" = no push happened.
	PushedSHA string
	// PushError: ensurePush failed before Deliver; Items were never attempted, so Deliver
	// should report this as each item's failure instead of attempting them.
	PushError string
	// ChecksSkipNote: display-ready note when GatePassed but no build/test check ran for a reason
	// worth telling the reader; "" says nothing.
	ChecksSkipNote string
	// IdempotencyKey: target artifact id + revision; "" with no backing artifact revision.
	IdempotencyKey string
}

// DeliverFunc: posts final staged delivery. Errors logged, never fail the node.
type DeliverFunc func(ctx context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error)

// DeliveryItemOutcome: one staged item's actual delivery result.
type DeliveryItemOutcome struct {
	Kind  string
	URL   string
	Error string
}

// PromptEventNeeded: true for A2A agents, false for llmagent.
func PromptEventNeeded(ag adkagent.Agent) bool {
	type nodeRunner interface {
		RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error]
	}
	_, ok := ag.(nodeRunner)
	return !ok
}

// maxContinueRounds: tool-bearing continuation rounds before tool-less fallback.
const maxContinueRounds = 4

const fetchSampleBytes = 300 // bytes of fetched content per URL for judge spot-checking

// workerActivity: worker's retrieval and workspace operations from session events.
type workerActivity struct {
	searches  []string
	fetched   map[string]struct{}
	seen      map[string]string
	staged    []memory.Candidate
	workspace []wsOp

	// recalled: recall_memory hits from a native worker's own calls, scanned from session events
	// (unlike an ACP worker's, a native worker's tool calls land in this session).
	recalled []memory.Delivered

	// sourceReads: ids of stored source artifacts (dispatch inputs, fetched pages) read successfully.
	sourceReads []string

	clonedRepos []string
	clonedDirs  []string
	paths       map[string]bool // successful fs ops paths, normalizePath'd

	written []string // jail-relative paths for buildChangedFilesSection

	// dataTools: rendered "tool(args) -> result" entries for calls with no
	// other judge-visible path (e.g. sleeper_matchup) - oldest first.
	dataTools []string

	// artifactsWritten: ids written/edited via a native artifact tool this round; feeds the
	// artifact-read zero-reads discard rule.
	artifactsWritten []string
	// rendered: render_ui surface and quiz key ids - kept out of artifactsWritten so
	// surface JSON (diff hunks with markdown links) is never scored as cited prose.
	rendered []string

	committed bool
	pushed    bool

	reviewCommented bool
	reviewSubmitted bool

	ranCommand bool

	answer string // final answer, set just before commitDelivery (a synthesizer's is its review body)

	stagedDelivery map[string]StagedDelivery
	currentBranch  string

	// skipArtifactRender: post stagedDelivery text as-is, never the code_review/pr_body artifact,
	// which may be stale (an aborted round's salvaged text, or an already-merged review).
	skipArtifactRender bool
	// ponytail: prefer plan.Setup's PR/issue number over ledger inference.
	prNumber int
}

// retrieved: a fetch, search result, clone, file read, or source-artifact read this session.
func (a workerActivity) retrieved() bool {
	return len(a.fetched) > 0 || len(a.seen) > 0 || len(a.clonedRepos) > 0 || len(a.paths) > 0 || a.readSource()
}

// readSource: a source-artifact read of something this node did not write itself.
func (a workerActivity) readSource() bool {
	for _, id := range a.sourceReads {
		if !slices.Contains(a.artifactsWritten, id) {
			return true
		}
	}
	return false
}

// ownArtifactIDs: artifacts this node wrote, rendered, read, or fetched as a page -
// visible to its judge and evidence whichever node's lineage they carry.
func (a workerActivity) ownArtifactIDs() map[string]bool {
	own := map[string]bool{}
	for _, ids := range [][]string{a.artifactsWritten, a.rendered, a.sourceReads} {
		for _, id := range ids {
			own[id] = true
		}
	}
	for u := range a.fetched {
		for _, v := range urlVariants(u) {
			if id, err := recordstore.IdentityFor(webPageKind, "", v); err == nil {
				own[id] = true
			}
		}
	}
	return own
}

// producedArtifacts: every artifact this round's judge must read before passing it.
func (a workerActivity) producedArtifacts() []string {
	return append(slices.Clone(a.artifactsWritten), a.rendered...)
}

// wsOp: one completed fs/git/run_command call/response pair.
type wsOp struct {
	tool   string
	detail string
	sample string
}

func contentPlainText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// recordSearchResults: extracts {url: snippet} from a batched web_search
// response's per-query result groups. First snippet wins.
func recordSearchResults(seen map[string]string, resp map[string]any) {
	if resp == nil {
		return
	}
	if queries, ok := resp["queries"].([]any); ok {
		for _, q := range queries {
			qm, ok := q.(map[string]any)
			if !ok {
				continue
			}
			recordSearchResultItems(seen, qm["results"])
		}
		return
	}
	// Legacy: a pre-batching response has "results" at the top level, no per-query grouping.
	recordSearchResultItems(seen, resp["results"])
}

// recordSearchResultItems: one query group's {url: snippet} extraction.
func recordSearchResultItems(seen map[string]string, results any) {
	items, ok := results.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		snippet, _ := m["snippet"].(string)
		seen[u] = strings.TrimSpace(trimToSample(snippet))
	}
}

// sortedStagedDelivery: sorted by target key for a stable delivery order.
func sortedStagedDelivery(staged map[string]StagedDelivery) []StagedDelivery {
	if len(staged) == 0 {
		return nil
	}
	keys := make([]string, 0, len(staged))
	for k := range staged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]StagedDelivery, 0, len(keys))
	for _, k := range keys {
		out = append(out, staged[k])
	}
	return out
}

// trimToSample: truncates to fetchSampleBytes at valid UTF-8 boundary.
func trimToSample(s string) string {
	if len(s) <= fetchSampleBytes {
		return s
	}
	s = s[:fetchSampleBytes]
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}
