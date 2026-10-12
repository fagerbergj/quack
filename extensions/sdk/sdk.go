// Package sdk is the quack extension API. It must never import github.com/fagerbergj/quack:
// quack imports this package and each extension package, never the reverse.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// ErrUnknownChat is Host.UpdateChatOrigin's error for a localID that never reached Dispatch
// or whose chat is gone. It is an expected case: log at Debug.
var ErrUnknownChat = errors.New("sdk: unknown chat")

// Extension is what a module provides to quack.
type Extension interface {
	// Tools join every agent's tool set; nil for an inbound-only extension.
	Tools() []tool.Tool

	// RegisterRoutes mounts inbound routes. authed sits behind quack's session auth;
	// public does not, so webhooks there must verify themselves.
	RegisterRoutes(authed chi.Router, public chi.Router)
}

// Starter is optional, for extensions that own background resources. Factories must stay
// side-effect free (they also run during config validation), so acquisition belongs here.
type Starter interface {
	Start(ctx context.Context) error
}

// UIDescriptor names an extension's entry point in the host's navigation. Href is a
// same-origin relative reference into the extension's own routes (e.g. "/remarkable/review").
type UIDescriptor struct {
	Title string
	Href  string

	// Icon is a Material Symbols name (preferred) or inline SVG; emoji fall back to the
	// generic extension glyph.
	Icon string
}

// UI is optional: an extension implementing it appears in the host's navigation;
// without it the extension is listed by name only.
type UI interface {
	UI() UIDescriptor
}

// UIKitCSS is the same-origin path to quack's extension design kit (.qk-page, .qk-card, ...).
// The v1 contract is frozen-additive: a breaking restyle ships at /v2/.
const UIKitCSS = "/assets/ext/v1/kit.css"

// RunStatus is the terminal outcome of a dispatched run, reported to RunObserver.
type RunStatus string

const (
	RunDone       RunStatus = "done"
	RunFailed     RunStatus = "failed"
	RunNeedsInput RunStatus = "needs_input"

	// RunCancelled means the user stopped the run. Answer may hold mid-thought partial text;
	// an observer must not deliver it as an answer.
	RunCancelled RunStatus = "cancelled"
)

// ArtifactSchemas is optional: artifact kind to JSON Schema. quack refuses a violating write
// and fails boot on a schema it cannot compile. Nil means nothing to validate.
type ArtifactSchemas interface {
	ArtifactSchemas() map[string]json.RawMessage
}

// RunObserver is optional and observation-only: RunEnded fires after a dispatched run's
// outcome is final and must never block or mutate the run.
type RunObserver interface {
	RunEnded(chatID string, outcome RunOutcome)
}

// RunOutcome is the terminal state of a dispatched run. Status is always set; the other
// fields only where Status makes them meaningful.
type RunOutcome struct {
	Status RunStatus

	// Answer is the run's final text; best-effort partial text when TimedOut.
	Answer string

	// Question and NodeID name the paused node's question when Status is RunNeedsInput.
	Question string
	NodeID   string

	// Error is the failed node's sanitized cause when Status is RunFailed; empty means a
	// failure with no known cause.
	Error string

	// PlanRan is false when a label/trigger-driven dispatch produced no plan at all; the
	// caller may re-dispatch once.
	PlanRan bool

	// TimedOut is true when the run hit RunConfig.Timeout before finishing.
	TimedOut bool
}

// Host is what quack hands an extension's Factory. Func fields other than Dispatch may be
// nil (unavailable or an older host); callers must nil-check and degrade.
type Host struct {
	// Dispatch starts a run, or appends a turn to one in progress (see ChatRef.LocalID).
	Dispatch DispatchFunc

	// Log is pre-tagged component=ext.<name>; log through it rather than a new handler.
	Log *slog.Logger

	// DataDir is extension-private persistent storage. quack guarantees only that the
	// directory exists and is this extension's alone.
	DataDir string

	// Version is the running quack build stamp (e.g. "0.51.26"); "" when unknown.
	Version string

	// PublicURL is the server's externally reachable base URL, for linking back to a run;
	// "" when unset, in which case omit the link.
	PublicURL string

	// Location is the user's configured zone (quack never sets time.Local). nil when
	// unconfigured: fall back to time.Local.
	Location *time.Location

	// ReadArtifact returns the latest bytes of a named input artifact in this chat, ok=false
	// when absent. user is the chat's ChatRef.User; the host may substitute the stored one.
	ReadArtifact func(chatID, user, name string) (data []byte, ok bool)

	// WriteArtifact saves a named input artifact and returns its 1-based per-name revision
	// and whether the bytes changed from the prior one. user is as for ReadArtifact.
	WriteArtifact func(chatID, user, name, mimeType string, data []byte) (revision int64, changed bool, err error)

	// ChatUser returns the ADK session identity a chat runs as, e.g. to re-engage a chat
	// with no fresh triggering user. ok is false for an unknown chatID.
	ChatUser func(chatID string) (user string, ok bool)

	// ArchiveChat archives a chat, e.g. on the extension's own "resolved" signal.
	ArchiveChat func(chatID string) error

	// UpdateChatOrigin refreshes a chat's provenance after dispatch (an issue closing, a PR
	// merging). localID is ChatRef.LocalID; ErrUnknownChat if it never reached Dispatch.
	UpdateChatOrigin func(localID string, origin ChatOrigin) error

	// InvalidateSetup says the branch a run was cloned from moved. quack decides whether a
	// refresh is safe and refuses rather than discard a node's work.
	InvalidateSetup func(chatID string) error

	// Classify is one free-text round trip to quack's judge model for an inline decision
	// before shaping a DispatchRequest. Not a run: no callback, delivery, or history.
	Classify func(ctx context.Context, prompt string) (string, error)

	// Decide asks the host's decision handler about a declared point (see DecisionPoints);
	// nil or an error means "no decision", proceed as before.
	Decide func(ctx context.Context, req DecideRequest) (Decision, error)
}

// DecisionQuestion is one typed question put to the decision handler.
type DecisionQuestion struct {
	Type         string // "noul" (true/false), "choice", or "score"
	Instructions string
	Criteria     any // the options for a choice, the levels for a score
}

// DecisionPoint declares one of an extension's points; the host namespaces
// Name as ext:<plugin>/<name> and rejects config or calls that don't match it.
type DecisionPoint struct {
	Name        string
	Description string
	Questions   map[string]DecisionQuestion
	Primary     string   // the question whose top answer drives Act and Restrict
	Restrictive []string // Primary answers that only narrow what the extension does
	Modes       []string // observe, guard, decide: what the extension acts on; empty = observe only
}

// DecisionPoints is optional: the points an extension may pass to Host.Decide.
// The host refuses an undeclared point.
type DecisionPoints interface {
	DecisionPoints() []DecisionPoint
}

// DecideRequest is the input to Host.Decide. The mode is host config; the extension supplies
// Baseline and acts only on Decision.Act or Decision.Restrict.
type DecideRequest struct {
	Point    string // a declared DecisionPoint's Name
	State    any    // the evidence the handler reads
	Baseline string // the extension's own answer to Primary, in its option space

	// ChatID is the chat the decision belongs to, for ledger attribution; the
	// host accepts only the plugin's own ext:<plugin>: chats. Empty = ctx's chat.
	ChatID string

	// Optional: when set, each must equal the declared point's.
	Questions   map[string]DecisionQuestion
	Primary     string
	Restrictive []string
}

// Decision is the handler's answer. Probabilities is keyed question then option.
type Decision struct {
	Top           string // most probable option of Primary
	TopP          float64
	Probabilities map[string]map[string]float64
	Outcome       string // "observe", "act", "fallback", or "restrict"
	Act           bool   // mode and confidence say to use Top in place of Baseline
	Restrict      bool   // a guard says to narrow, never widen
}

// DispatchFunc starts or continues a run (see ChatRef.LocalID).
type DispatchFunc func(ctx context.Context, req DispatchRequest) error

// DispatchRequest is the entire surface an extension has to influence a run, decided once
// before the workflow starts: there are no agent-loop hooks.
type DispatchRequest struct {
	Chat     ChatRef
	Ask      Ask
	Run      RunConfig
	Delivery DeliveryAuthority
}

// ChatRef identifies and titles the chat a dispatch targets.
type ChatRef struct {
	// LocalID is extension-scoped ("doc-42"); quack namespaces it as "ext:<extension>:<localID>".
	// A LocalID that already has a chat gets a new turn; an unseen one starts a chat.
	LocalID string

	// User is the ADK session identity the run executes as.
	User string

	// Title is applied only if the chat has no title yet.
	Title string

	// Origin is sidebar provenance; nil renders no origin chip.
	Origin *ChatOrigin

	// ResetHistory clears the chat's prior turns before this one is appended.
	ResetHistory bool
}

// SubjectState is the typed lifecycle state of a ChatOrigin's subject, so hosts never parse
// Badge's display text. "" means unknown or not applicable.
type SubjectState string

const (
	SubjectOpen   SubjectState = "open"
	SubjectMerged SubjectState = "merged"
	SubjectClosed SubjectState = "closed"
)

// ChatOrigin is generic provenance the SPA renders without knowing the extension: a label
// chip, optional badge and link, and grouping by Kind and Labels.
type ChatOrigin struct {
	Extension string // registration name, e.g. "remarkable"
	Label     string // human handle, e.g. the doc title or "owner/repo#42"
	Kind      string // grouping category, e.g. "document", "pr", "issue"
	Href      string // the one navigable link for the chat's subject
	Badge     string // optional short status chip, e.g. "draft"

	// State is the machine-readable subject state. Badge is display-only; hosts must never
	// branch on it.
	State SubjectState

	// Labels are extra grouping dimensions (repo, folder, tags), one sidebar section per key;
	// slice-valued because one chat can carry several values on a dimension.
	Labels map[string][]LabelValue
}

// LabelValue is one value within a ChatOrigin.Labels dimension.
type LabelValue struct {
	Value   string // raw value; what matching/counting keys on
	Display string // display text; "" falls back to Value
	Href    string // optional link-out for THIS value; "" = no link
}

// Ask is what the dispatched run is being asked to do.
type Ask struct {
	// Message is the turn content the planner sees.
	Message string

	// NodeContext is per-node background text, injected into a specific node's task rather
	// than the planner-scoped Message.
	NodeContext string

	// Attachments are delivered alongside Message.
	Attachments []Attachment

	// ContextItems are name-keyed details a node's task may reference by name.
	ContextItems []NamedContext
}

// NamedContext is one name-keyed piece of context a node's task text may
// reference by Name.
type NamedContext struct {
	Name   string
	Detail string
}

// Attachment is a byte payload delivered with a DispatchRequest.
type Attachment struct {
	Name string
	MIME string
	Data []byte
}

// RunConfig controls how the dispatched run executes.
type RunConfig struct {
	// Workflow names a workflow-catalog shape, e.g. "document-ingest". Empty selects
	// quack's default planner-driven flow.
	Workflow string

	// ReadOnly forces every node read-only, with no delivery target.
	ReadOnly bool

	// Setup is pre-clone coordinates; nil means no pre-provisioned clone.
	Setup *Setup

	// Timeout bounds this run's execution, not queue wait; zero means no per-run bound.
	Timeout time.Duration
}

// Setup is the pre-provisioned clone a run should use.
type Setup struct {
	Repo       string
	BaseRef    string
	WorkBranch string

	// ExistingHeadRef names a branch already on Repo to check out as-is instead of creating
	// WorkBranch from BaseRef (e.g. a PR's head branch). It overrides WorkBranch.
	ExistingHeadRef string
}

// DeliveryKind is quack's closed vocabulary of deliverable items; typed so a mismatch
// between quack and an extension is a compile error, not silent non-delivery.
type DeliveryKind string

const (
	KindCommit  DeliveryKind = "commit"
	KindPR      DeliveryKind = "pull_request"
	KindReview  DeliveryKind = "review"
	KindComment DeliveryKind = "comment"
)

// DeliveryAuthority declares which delivery kinds a run may use.
type DeliveryAuthority struct {
	// AllowedKinds nil = unrestricted; non-nil empty = deny all. The extension resolves its
	// own permission logic to this flat list before dispatch.
	AllowedKinds []DeliveryKind
}

// CallInfo is the run a tool call belongs to. Advisory for gate-backed
// deliveries; a tool that acts directly (posts, writes) must enforce it itself.
type CallInfo struct {
	// ChatID is quack's chat id; "ext:<extension>:<ChatRef.LocalID>" for a chat
	// the extension dispatched.
	ChatID string

	// UserID is the chat's user: what the extension put in ChatRef.User for its own chats.
	UserID string

	// AllowedDeliveryKinds is the run's grant, as DeliveryAuthority.AllowedKinds:
	// nil = no grant governs this run (unrestricted), non-nil empty = deny all.
	AllowedDeliveryKinds []DeliveryKind

	// ReadOnly marks a plan-only run: it must not write or post.
	ReadOnly bool
}

type callInfoKey struct{}

// WithCallInfo is host-side: set it on the ctx a tool's Run receives (the runner
// ctx, or a per-call overlay), and only when the run's grant is known.
func WithCallInfo(ctx context.Context, ci CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, ci)
}

// CallInfoFrom reads the host's CallInfo from a tool's agent.Context (or any
// ctx derived from it); ok is false when the host predates CallInfo.
func CallInfoFrom(ctx context.Context) (CallInfo, bool) {
	ci, ok := ctx.Value(callInfoKey{}).(CallInfo)
	return ci, ok
}

// --- Inverse capabilities: quack calls into the extension. ---

// Deliverer is optional: quack hands it staged items it has already gated and pushed.
type Deliverer interface {
	Deliver(ctx context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error)
}

// DeliveryContext is everything a Deliverer needs to turn staged items into
// calls against its own external system.
type DeliveryContext struct {
	NodeID string
	ChatID string
	Items  []StagedDelivery

	CloneURL string

	// PushedSHA proves quack already pushed Branch; the extension never touches quack's clone.
	PushedSHA string

	// PushError is set when quack's own push failed. Deliverers must not attempt Items and
	// should report this as each item's failure.
	PushError string

	Branch string

	IssueNumber int

	GatePassed     bool
	GateFeedback   string
	ChecksSkipNote string

	// IdempotencyKey (target artifact id + revision) lets DeliveryRecoverer find a post after a
	// crash, e.g. via a hidden marker. "" from an older host.
	IdempotencyKey string
}

// StagedDelivery is one item quack has gated and is ready to deliver.
type StagedDelivery struct {
	Kind DeliveryKind

	Branch string
	Title  string
	Body   string

	TitleOmitted bool
	BodyOmitted  bool

	// Event and Slot are opaque to quack (judge-prompt label text and half a dedup key); the
	// extension owns their vocabulary.
	Event string
	Slot  string

	Comments []ReviewComment

	Recovered bool
}

// ReviewComment is one inline comment on a staged review.
type ReviewComment struct {
	Path string
	Line int
	Body string
	// Severity is the host's label for the finding (blocking, suggestion,
	// nit, question); empty when the host did not classify it.
	Severity string
}

// DeliveryItemOutcome reports what happened when one staged item was
// delivered.
type DeliveryItemOutcome struct {
	Kind  string
	URL   string
	Error string
}

// DeliveryRecoverer is optional: after a crash between delivery intent and done, it looks the
// idempotency key up at the target and reports whether it was posted, never posting again.
type DeliveryRecoverer interface {
	RecoverDelivery(ctx context.Context, key string, dc DeliveryContext) (found bool, outcome DeliveryItemOutcome, err error)
}

// GitCredentialSource is optional: credentials for cloning or pushing to the extension's own
// remotes, used for the initial clone and quack's push before Deliver.
type GitCredentialSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// GitCredential is a credential for one git remote host.
type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// Assignment is one bit of work in a dispatched plan, passed to the two
// node-reuse hooks below. Mirrors quack's own internal dag.Assignment.
type Assignment struct {
	PlanID    string
	NodeID    string
	Agent     string
	Task      string
	DependsOn []string
	ContextID string
	TaskID    string

	// Meta is keyed by extension name: Meta[name] holds only that extension's own
	// OnAssignment return value.
	Meta map[string]map[string]any
}

// AssignmentMetaExtension is an optional interface: OnAssignment stamps a
// per-extension map into Assignment.Meta[<this extension's name>] once per assignment.
type AssignmentMetaExtension interface {
	OnAssignment(ctx context.Context, a Assignment) map[string]any
}

// AssignmentFreshnessChecker is an optional interface: BeforeAssignment
// judges a REUSED node fresh or stale before quack resumes its session.
type AssignmentFreshnessChecker interface {
	BeforeAssignment(ctx context.Context, a Assignment) (fresh bool, reason string)
}

// Factory builds an extension from the raw bytes of its extensions.<name> block. It must be
// side-effect free: validate and construct only (see Starter).
type Factory func(host Host, config []byte) (Extension, error)

// BaseConfig is the keys quack reads from every extensions.<name> block before Factory sees the
// same bytes; "enabled" and "data_dir" are reserved, so extension configs must not redefine them.
type BaseConfig struct {
	// Enabled defaults to true when the block exists, so a deployment can disable a module
	// without deleting its config.
	Enabled *bool `yaml:"enabled"`

	// DataDir overrides Host.DataDir's default (<workspace>/extensions/<name>) when non-empty.
	DataDir string `yaml:"data_dir"`
}
