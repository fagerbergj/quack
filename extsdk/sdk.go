// Package sdk is the quack extension API. It is self-contained: it must
// never import github.com/fagerbergj/quack, so an extension written against
// it can never accidentally depend on quack internals. quack imports this
// package (and each extension package) - never the reverse.
//
// Design principles that shape every type below:
//   - Dispatch-time shaping only: DispatchRequest is the entire surface an
//     extension has to influence a run. There are no agent-loop hooks -
//     enforcement stays gate-owned inside quack.
//   - Ownership test for strings: a field stays a plain string only if
//     quack-core never interprets it, only passes it through or matches it
//     opaquely. A string quack-core parses, branches on, or must keep in
//     sync with the extension becomes a named type (DeliveryKind, RunStatus)
//     so a typo is a compile error, not silent behavior loss.
//   - RunObserver is observation-only: it never blocks or mutates a run,
//     and fires only after the run's outcome is final.
//   - Factories must be side-effect free: validate config and construct,
//     nothing more - they also run in contexts that never serve, such as
//     config checks. Background resources belong in Start/Stop.
package sdk

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// Extension is what a module provides to quack.
type Extension interface {
	// Tools join every agent's tool set. Return nil/empty for an
	// inbound-only extension (routes/dispatch, no tools).
	Tools() []tool.Tool

	// RegisterRoutes mounts the extension's inbound routes. authed sits
	// behind quack's session auth (UI pages, extension APIs); public does
	// not (webhooks, which own their own verification - see Starter/Stopper
	// doc for the shared precedent). Either router may be left unused.
	RegisterRoutes(authed chi.Router, public chi.Router)
}

// Starter is an optional lifecycle interface for extensions that own
// background resources (watchers, pollers). Factories themselves must stay
// side-effect free - they also run in contexts that never serve, such as
// config validation - so resource acquisition belongs in Start, not the
// Factory.
type Starter interface {
	Start(ctx context.Context) error
}

// Stopper is the Starter counterpart. Stop must be idempotent: quack may
// call it during shutdown paths that don't guarantee Start ran, or don't
// guarantee Stop runs exactly once.
type Stopper interface {
	Stop(ctx context.Context) error
}

// UIDescriptor names an extension's entry point in the host's navigation.
// Href is a same-origin relative reference into the extension's own routes
// (e.g. "/remarkable/review").
type UIDescriptor struct {
	Title string
	Href  string
}

// UI is an optional interface: an extension implementing it appears in the
// host's navigation (quack's GET /api/v1/extensions). Without it the
// extension is listed by name only.
type UI interface {
	UI() UIDescriptor
}

// RunStatus is the terminal outcome of a dispatched run, reported to
// RunObserver.
type RunStatus string

const (
	RunDone       RunStatus = "done"
	RunFailed     RunStatus = "failed"
	RunNeedsInput RunStatus = "needs_input"
)

// RunObserver is an optional, observation-only interface: quack calls
// RunEnded after a dispatched run's outcome is final, so an extension can
// mark its own records done/failed and drive retries. It never blocks or
// mutates the run - dispatch-time shaping (DispatchRequest) is the only
// place an extension influences a run; there are no agent-loop hooks.
type RunObserver interface {
	RunEnded(chatID string, outcome RunOutcome)
}

// RunOutcome is the terminal state of a dispatched run. Status is always
// set; the rest is populated to the extent that status makes them
// meaningful (Question/NodeID only for RunNeedsInput, Answer best-effort
// partial text when TimedOut).
type RunOutcome struct {
	Status RunStatus

	// Answer is the run's final text; a best-effort partial answer if
	// TimedOut is true.
	Answer string

	// Question is the paused node's question when Status is
	// RunNeedsInput.
	Question string

	// NodeID is which node paused when Status is RunNeedsInput.
	NodeID string

	// PlanRan is false when a label/trigger-driven dispatch produced no
	// plan at all - the caller may choose to re-dispatch once.
	PlanRan bool

	// TimedOut is true when the run hit its time budget before finishing.
	TimedOut bool
}

// Host is what quack hands an extension's Factory.
type Host struct {
	// Dispatch starts a run, or appends a turn to one already in progress -
	// see ChatRef.LocalID.
	Dispatch DispatchFunc

	// Log is pre-tagged component=ext.<name>; extensions should log through
	// it rather than building their own handler.
	Log *slog.Logger

	// DataDir is extension-private persistent storage, e.g. a SQLite
	// registry mapping external documents to chat ids. Extensions own its
	// contents; quack only guarantees the directory exists and is theirs
	// alone.
	DataDir string

	// EnsureContextDir allocates (creating if needed) a sandboxed directory
	// alongside a dispatched run's own clone, for evidence files too large
	// or too raw for Ask.Message/ContextItems - the extension writes into it
	// directly; quack only guarantees the path is sandboxed for chatID.
	EnsureContextDir func(userID, chatID string) (string, error)

	// ChatUser returns the ADK session identity a chat is running as -
	// e.g. recovering the acting user when an extension re-engages a chat
	// with no fresh triggering user available. ok is false for an unknown
	// chatID.
	ChatUser func(chatID string) (user string, ok bool)

	// ArchiveChat archives a chat, e.g. on the extension's own "this is
	// resolved" signal.
	ArchiveChat func(chatID string) error

	// Classify is a single free-text model round trip - a classification or
	// short judgment call an extension needs INLINE, before it decides how
	// to shape a DispatchRequest (e.g. "is this comment asking for work, or
	// just conversation?"). It is not a dispatched, gated, observed run: no
	// RunObserver callback, no delivery, no session history - just a prompt
	// in, an answer out, bound to quack's judge/advisor model. nil is a
	// valid value (no judge model configured); callers must degrade
	// gracefully, matching every other best-effort Host call.
	Classify func(ctx context.Context, prompt string) (string, error)

	// UpdateChatOrigin lets an extension update a chat's origin without
	// starting a run — used when the extension handles state-change events
	// from its upstream system (e.g. issues.closed, pull_request.merged or
	// reopened) that need to refresh the chat's sidebar badge. The origin
	// parameter is nil to remove an existing origin, or a ChatOrigin struct
	// with updated Badge and labels. Extensions marshal it before passing
	// — quack stores it as JSON. nil func means no update path configured;
	// callers should degrade gracefully.
	UpdateChatOrigin func(ctx context.Context, chatID string, origin *ChatOrigin) error
}

// DispatchFunc starts or continues a run. See ChatRef.LocalID for the
// new-chat-vs-append-a-turn semantics.
type DispatchFunc func(ctx context.Context, req DispatchRequest) error

// DispatchRequest is the dispatch-time shape of a run, grouped by concern:
// where it goes (Chat), what it asks (Ask), how it runs (Run), and what it
// may deliver (Delivery). This is the entire surface an extension has to
// influence a run - there are no agent-loop hooks (see the package doc):
// everything here is decided once, before the workflow starts.
type DispatchRequest struct {
	Chat     ChatRef
	Ask      Ask
	Run      RunConfig
	Delivery DeliveryAuthority
}

// ChatRef identifies and titles the chat a dispatch targets.
type ChatRef struct {
	// LocalID is a stable identity chosen by the extension, scoped to that
	// extension - e.g. "doc-42", not "remarkable-doc-42". quack namespaces
	// it into the global chat id as "ext:<extension>:<localID>", so two
	// extensions (or an extension and a user chat) can never collide.
	// Dispatching to a LocalID that already has a chat appends a turn to
	// it (the GitHub webhook's resume semantics); one quack hasn't seen
	// starts a fresh chat.
	LocalID string

	// User is the ADK session identity the run executes as.
	User string

	// Title is applied only if the chat has no title yet.
	Title string

	// Origin is sidebar provenance: how the chat should present itself and
	// group with others from the same extension. Nil is fine - the chat
	// just renders with no origin chip.
	Origin *ChatOrigin

	// ResetHistory clears the chat's prior turns before this one is
	// appended, replacing v1's Runner.ResetSession call.
	ResetHistory bool
}

// ChatOrigin is generic provenance the SPA renders without knowing the
// extension that produced it: a label chip, an optional badge and external
// link, and grouping by extension/kind/whatever dimensions Labels carries.
type ChatOrigin struct {
	Extension string // registration name, e.g. "remarkable"
	Label     string // human handle, e.g. the doc title or "owner/repo#42"
	Kind      string // grouping category, e.g. "document", "pr", "issue"
	// Href is the ONE navigable link for the chat's subject. Named for the
	// role (a hypermedia reference), not the datatype - a plain URL type
	// would say what it IS, not what it's FOR.
	Href  string
	Badge string // optional short status chip, e.g. "draft"

	// Labels carries extra grouping dimensions beyond Kind - repo, state,
	// folder, tags, whatever the extension's domain needs - keyed for
	// grouping (the sidebar renders one labeled section per key) and
	// slice-valued because a single chat can carry several values on one
	// dimension (e.g. a document with multiple tags). Named for the domain
	// word extensions actually deal in (GitHub labels, reMarkable tablet
	// tags), not frontend rendering jargon.
	Labels map[string][]LabelValue
}

// LabelValue is one value within a ChatOrigin.Labels dimension. Display
// (not Label) so a Labels[...] value's own display text doesn't stutter
// against the map it lives in.
type LabelValue struct {
	Value   string // raw value; what matching/counting keys on
	Display string // display text; "" falls back to Value
	Href    string // optional link-out for THIS value; "" = no link
}

// Ask is what the dispatched run is being asked to do.
type Ask struct {
	// Message is the turn content the planner sees.
	Message string

	// NodeContext is per-node background text - a narrower ask than
	// Message, injected into a specific node's task rather than the
	// planner-scoped evidence Message carries.
	NodeContext string

	// Attachments are delivered alongside Message.
	Attachments []Attachment

	// ContextItems are name-keyed detail a node's task may reference by
	// name - e.g. one failing check's detail, one lint finding's detail.
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
	// Workflow names a workflow-catalog shape, e.g. "document-ingest".
	// Empty selects quack's default planner-driven flow.
	Workflow string

	// ReadOnly forces every node read-only, with no delivery target.
	ReadOnly bool

	// Setup is pre-clone coordinates; nil means no pre-provisioned clone.
	Setup *Setup

	// Timeout bounds this run's execution (not queue wait); zero means no
	// per-run bound. Populating RunOutcome.TimedOut needs this: without a
	// deadline scoped to the dispatch itself, "did this run time out" has
	// nothing to compare against.
	Timeout time.Duration
}

// Setup is the pre-provisioned clone a run should use.
type Setup struct {
	Repo       string
	BaseRef    string
	WorkBranch string

	// ExistingHeadRef, when non-empty, names a branch that already exists on
	// Repo and must be checked out as-is instead of creating WorkBranch fresh
	// from BaseRef - e.g. resuming work on a pull request's own head branch.
	// It overrides WorkBranch for the checkout, not just supplements it.
	ExistingHeadRef string
}

// DeliveryKind is the closed vocabulary of things a run can stage for
// delivery. It is core-owned (quack stages delivery items; the extension
// only allowlists kinds), so it is a typed constant rather than an open
// string - two parties matching untyped strings means a typo is silent
// non-delivery.
type DeliveryKind string

const (
	KindCommit  DeliveryKind = "commit"
	KindPR      DeliveryKind = "pull_request"
	KindReview  DeliveryKind = "review"
	KindComment DeliveryKind = "comment"
)

// DeliveryAuthority declares which delivery kinds a run may use.
type DeliveryAuthority struct {
	// AllowedKinds nil = unrestricted (today's nil-Grant semantics);
	// non-nil empty = deny all. The extension resolves its own permission
	// logic (labels, authorship, scope) down to this flat list before
	// dispatch.
	AllowedKinds []DeliveryKind
}

// --- Inverse capabilities: quack calls into the extension. ---

// Deliverer is an optional interface an extension implements to receive
// staged delivery items quack has already gated and pushed. Detected via
// type assertion on the registered Extension, the same pattern as
// Starter/Stopper.
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

	// PushedSHA is proof quack already pushed Branch before calling
	// Deliver - the extension never touches quack's clone directory
	// itself.
	PushedSHA string

	Branch string

	IssueNumber int

	GatePassed     bool
	GateFeedback   string
	ChecksSkipNote string
}

// StagedDelivery is one item quack has gated and is ready to deliver.
type StagedDelivery struct {
	Kind DeliveryKind

	Branch string
	Title  string
	Body   string

	TitleOmitted bool
	BodyOmitted  bool

	// Event and Slot are opaque to quack-core: Event is dropped into the
	// judge prompt as label text (never parsed against an enum), Slot is
	// used only as half of a dedup key. Both stay plain strings - the
	// extension owns their vocabulary.
	Event string
	Slot  string

	Comments []ReviewComment

	Recovered bool
}

// ReviewComment is one inline comment on a staged review. Path/Line earn
// their place as structural fields (not opaque strings) because quack-core
// itself formats them for the judge's findings report.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// DeliveryItemOutcome reports what happened when one staged item was
// delivered.
type DeliveryItemOutcome struct {
	Kind  string
	URL   string
	Error string
}

// GitCredentialSource is an optional interface an extension implements to
// hand quack credentials for cloning or pushing to its own remotes. Used
// both for the initial clone and for quack's own push before Deliver is
// called.
type GitCredentialSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// GitCredential is a credential for one git remote host.
type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// Factory builds an extension. config is the raw bytes of the deployment's
// extensions.<name> block - the extension unmarshals its own config; quack
// treats extension blocks as opaque beyond the reserved BaseConfig keys
// (see its doc comment). Factories must be side-effect free: validate
// config and construct, nothing more (see Starter/Stopper).
type Factory func(host Host, config []byte) (Extension, error)

// BaseConfig is the small set of keys quack itself reads from every
// extensions.<name> block before handing the same raw bytes to Factory -
// "enabled" and "data_dir" are reserved: an extension's own config struct
// should not redefine them (yaml silently ignores unknown fields, so
// leaving them out of an extension's struct is enough).
type BaseConfig struct {
	// Enabled, read from the "enabled" key, defaults to true when the
	// extensions.<name> block exists at all - so a deployment can disable
	// a module (dormant, exactly like an absent block) without deleting
	// its config.
	Enabled *bool `yaml:"enabled"`

	// DataDir, read from the "data_dir" key, overrides Host.DataDir's
	// default (<workspace>/extensions/<name>) when non-empty.
	DataDir string `yaml:"data_dir"`
}
