// Package recordstore is a typed record store over the ADK artifact.Service
// (#1090 P2). A record is identified by a kind/instance id; the id is never
// composed by a caller - the kind registry derives it from the saved content
// (via the kind's registered Identity func) and Save returns it. Two content
// classes share the store: structured (JSON, validated against a registered
// kind) and blob (raw bytes + mime, e.g. markdown/text/PDF). Kind naming,
// schemas, and identity functions live with each record type's own package
// (e.g. internal/vetting/reviewrecord.go); recordstore only holds the registry.
package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// isNotFound reports whether err is the artifact.Service "no such
// artifact/version" sentinel, which every backend wraps around fs.ErrNotExist.
func isNotFound(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// idSep joins kind/instance (design's logical shape uses "/", but ADK's
// artifact.Service rejects "/" and "\" in FileName - validateFileName - so
// the physical record id substitutes ":". An instance value may itself
// contain ":" (e.g. "pr:123"); KindOf/id only ever split on the first
// separator, so that's safe.
const idSep = ":"

// KindOf extracts the kind segment from an id, "" if malformed.
func KindOf(id string) string {
	kind, _, ok := strings.Cut(id, idSep)
	if !ok {
		return ""
	}
	return kind
}

// Class is an artifact's content class.
type Class string

const (
	Structured Class = "structured" // JSON body, validated against the registered kind
	Blob       Class = "blob"       // raw bytes + mime, no validation beyond size
)

// IdentityFunc derives a kind's instance segment from the content being
// saved (already marshaled to bytes) and an optional hint the caller
// supplies (e.g. the chat's external subject identity) - never from a
// caller-composed string. A finding's identity ignores hint entirely and
// hashes fields inside content, so it comes out the same regardless of
// which node or hint produced it.
type IdentityFunc func(content []byte, hint string) (instance string, err error)

// KindSpec is one registered kind's shape: content class, schema version and
// JSON Schema text (for #1091's generated write_<kind> tools - "" for a blob
// kind, which has no schema), a validator (nil for a blob kind), and the
// identity function that derives its instance segment.
type KindSpec struct {
	Class         Class
	SchemaVersion int
	JSONSchema    string // #1091 tool generation input; the write_<kind> tool's input schema verbatim
	Validate      func(json.RawMessage) error
	Identity      IdentityFunc
	// RequiresHint declares that Identity fails without a non-empty hint
	// (the requireHint pattern) - callers that can't supply a real session
	// hint (e.g. write_artifact/write_<kind> for an arbitrary caller-chosen
	// kind) use this to decide whether to pass one at all, rather than
	// passing a hint unconditionally and corrupting a hint-optional kind's
	// content-hash identity (#1108 finding 2).
	RequiresHint bool

	name string // set only by Kinds(); not part of the registered spec
}

// Name is the registered kind name (populated on values returned by Kinds()).
func (k KindSpec) Name() string { return k.name }

var (
	registryMu sync.RWMutex
	registry   = map[string]KindSpec{}
)

// Register declares kind's shape (§4.3/§4.4). Call once (package init) per
// kind from the record type's own package - #1090 P2's registered kinds are
// code_review, finding, document, pr_body, text, bytes. Panics on a
// duplicate registration, a spec missing Identity (a wiring bug, not a
// runtime error - every kind must derive its own id), or a JSONSchema that
// doesn't parse - the write_<kind> tool generators (MCP and ADK) both trust
// this schema is valid and previously skipped the tool silently on either
// surface when it wasn't; failing here instead means both surfaces fail the
// same way, loudly, at startup (#1108 finding 3).
func Register(kind string, spec KindSpec) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[kind]; ok {
		panic("recordstore: kind " + kind + " already registered")
	}
	if spec.Identity == nil {
		panic("recordstore: kind " + kind + " registered with no Identity func")
	}
	if spec.JSONSchema != "" {
		var schema jsonschema.Schema
		if err := json.Unmarshal([]byte(spec.JSONSchema), &schema); err != nil {
			panic("recordstore: kind " + kind + " registered with invalid JSONSchema: " + err.Error())
		}
	}
	registry[kind] = spec
}

// SpecFor returns kind's registered spec, or ok=false if it isn't registered -
// exported so a generic write path (write_artifact) can check RequiresHint
// for a caller-chosen kind before deciding whether to pass one.
func SpecFor(kind string) (spec KindSpec, ok bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	spec, ok = registry[kind]
	return spec, ok
}

func lookupKind(kind string) (KindSpec, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	spec, ok := registry[kind]
	if !ok {
		return KindSpec{}, fmt.Errorf("recordstore: kind %q is not registered", kind)
	}
	return spec, nil
}

// Lineage is the per-revision provenance envelope (#1090 §4.2), stamped on
// the store row rather than inside the bytes - so blob kinds carry it too.
// NodeID is provenance only (the node that authored this revision), never
// part of the artifact's id.
type Lineage struct {
	NodeID         string `json:"node_id"`
	Round          int    `json:"round"`
	ParentRevision int    `json:"parent_revision"`
	// BaseRevision is the revision the editor last read (Edit's caller-supplied
	// base_revision) - advisory only, recorded for observability; the merge
	// itself always targets whatever is actually latest (see Edit).
	BaseRevision      int       `json:"base_revision"`
	TriggerAnnotation string    `json:"trigger_annotation,omitempty"`
	HeadSHA           string    `json:"head_sha,omitempty"`
	SavedAt           time.Time `json:"saved_at"`
	Author            string    `json:"author"`
	// TurnID targets the store row's existing turn_id column (internal/store's
	// TurnAwareService.SaveForTurn concept), not the lineage JSON blob -
	// excluded from marshaling so it isn't duplicated in both places.
	TurnID string `json:"-"`
}

// metaSaver/metaLoader are implemented by *store.TurnAwareService (checked
// structurally, so recordstore never imports internal/store). A plain
// artifact.Service without them - artifact.InMemoryService(), used by most
// tests - still saves/loads fine; kind/class/lineage just don't persist.
type metaSaver interface {
	SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error)
}
type metaLoader interface {
	LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error)
}

// Client scopes record reads/writes to one (appName, userID, sessionID).
type Client struct {
	svc                        artifact.Service
	appName, userID, sessionID string
	// ledgerStore: the WAL's fail-closed AppendIntent path (#1090 §4.9/#1100).
	// nil = no WAL; Save* behaves exactly as before #1100. Set only via
	// WithLedger, by a caller that has already restricted it to a
	// transactional (postgres) backend - see vetting.Config.Ledger's doc.
	ledgerStore ledger.LedgerStore
}

// New scopes a client to one session over svc (the artifact.Service the
// concrete store wraps, e.g. *store.TurnAwareService).
func New(svc artifact.Service, appName, userID, sessionID string) *Client {
	return &Client{svc: svc, appName: appName, userID: userID, sessionID: sessionID}
}

// WithLedger arms the fail-closed WAL path on c and returns c. store should
// already be filtered to a transactional backend by the caller (see
// vetting.Config.Ledger) - recordstore itself doesn't inspect the backend
// kind, it just trusts a non-nil store to make AppendIntent atomic.
func (c *Client) WithLedger(store ledger.LedgerStore) *Client {
	c.ledgerStore = store
	return c
}

// revisionLocks/revisionLockFor: a process-local per-(chat,id) mutex, held
// around read-parent + AppendIntent + saveRow so the WAL's next revision and
// the store's assigned revision are computed under the same lock. The key is
// deliberately COARSER than the store's own four-part lock key (appName +
// userID + sessionID + name, internal/store/artifact.go's
// artifactRevisionLocks) - sessionID (chat) + id is a superset of what the
// store serializes. Do not "fix" this toward the finer key: two different
// users' saves under the same (chat, id) must still serialize here, or their
// WAL entries can race the same way a same-user race would, breaking the
// parent chain.
var revisionLocks sync.Map // key -> *sync.Mutex

func revisionLockFor(chatID, id string) *sync.Mutex {
	v, _ := revisionLocks.LoadOrStore(chatID+"\x00"+id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// lastRevision returns the highest MATERIALIZED revision recorded in an
// artifact.revision WAL entry for id, 0 if none. A retried save after a
// wedge (see KindArtifactRevisionAborted) reuses the SAME revision number
// its failed attempt claimed, so "aborted" is a per-revision-number STATUS
// that later entries can flip back to materialized, not a permanent
// exclusion - entries are walked in seq order and the last word for a given
// revision number wins. A full per-chat scan (ponytail: O(entries);
// ledger_entries has no (chat_id, key) index yet, so a server-side key
// filter would still scan - the index belongs with #1101's projections work).
func lastRevision(ctx context.Context, store ledger.LedgerStore, chatID, id string) (int, error) {
	entries, err := store.ReadEntries(ctx, chatID, 0)
	if err != nil {
		return 0, err
	}
	materialized := map[int]bool{} // revision -> does the LATEST entry for it count?
	var payload struct {
		Revision int `json:"revision"`
	}
	for _, e := range entries { // seq order: later entries override earlier ones for the same revision
		if e.Key != id {
			continue
		}
		switch e.Kind {
		case ledger.KindArtifactRevision:
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			materialized[payload.Revision] = true
		case ledger.KindArtifactRevisionAborted:
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			materialized[payload.Revision] = false
		}
	}
	rev := 0
	for r, ok := range materialized {
		if ok && r > rev {
			rev = r
		}
	}
	return rev, nil
}

// abortedRevisionPayload is the artifact.revision.aborted entry's payload.
type abortedRevisionPayload struct {
	Revision int    `json:"revision"`
	Reason   string `json:"reason"`
}

// appendAborted records that revision never materialized (saveRow failed
// after the artifact.revision intent already landed) - best-effort: this is
// cleanup for lastRevision's own bookkeeping, not itself part of the
// fail-closed contract, so a failure here is Warn-logged, not returned.
func appendAborted(ctx context.Context, store ledger.LedgerStore, chatID, turnID, nodeID, id string, revision int, reason string) {
	payload, err := json.Marshal(abortedRevisionPayload{Revision: revision, Reason: reason})
	if err != nil {
		slog.Warn("recordstore: marshal artifact.revision.aborted payload failed", "component", "recordstore", "id", id, "revision", revision, "err", err)
		return
	}
	if _, err := store.AppendIntent(ctx, ledger.Entry{
		ChatID: chatID, TurnID: turnID, NodeID: nodeID,
		Kind: ledger.KindArtifactRevisionAborted, Key: id, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		slog.Warn("recordstore: append artifact.revision.aborted failed; the id may stay wedged until this is retried", "component", "recordstore", "id", id, "revision", revision, "err", err)
	}
}

// artifactRevisionPayload is the artifact.revision WAL entry's payload
// (#1090 §4.9): bytes_ref is the store row's key (the id), never the bytes,
// so a large blob is one small entry.
type artifactRevisionPayload struct {
	ID             string  `json:"id"`
	Revision       int     `json:"revision"`
	ParentRevision int     `json:"parent_revision"`
	Kind           string  `json:"kind"`
	Class          Class   `json:"class"`
	Lineage        Lineage `json:"lineage"`
	BytesRef       string  `json:"bytes_ref"`
}

func (c *Client) save(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage) (int, error) {
	if c.ledgerStore != nil {
		// Held across read-parent + AppendIntent + saveRow: otherwise two
		// concurrent saves for the same id can both read the same parent and
		// both claim the same next revision in the WAL (adversarial review
		// finding on #1100).
		mu := revisionLockFor(c.sessionID, id)
		mu.Lock()
		defer mu.Unlock()
		parentRev, err := lastRevision(ctx, c.ledgerStore, c.sessionID, id)
		if err != nil {
			return 0, fmt.Errorf("recordstore: read ledger parent revision for %s: %w", id, err)
		}
		if parentRev != lineage.ParentRevision {
			slog.Warn("recordstore: ledger parent_revision disagrees with caller-tracked revision", "component", "recordstore", "id", id, "ledger_parent", parentRev, "caller_parent", lineage.ParentRevision)
		}
		lineage.ParentRevision = parentRev // ledger is read, never computed (#1090 §4.9)
		nextRev := parentRev + 1
		payload, err := json.Marshal(artifactRevisionPayload{ID: id, Revision: nextRev, ParentRevision: parentRev, Kind: kind, Class: class, Lineage: lineage, BytesRef: id})
		if err != nil {
			return 0, fmt.Errorf("recordstore: marshal artifact.revision payload for %s: %w", id, err)
		}
		if _, err := c.ledgerStore.AppendIntent(ctx, ledger.Entry{
			ChatID: c.sessionID, TurnID: lineage.TurnID, NodeID: lineage.NodeID,
			Kind: ledger.KindArtifactRevision, Key: id, At: time.Now().UTC(), Payload: payload,
		}); err != nil {
			// Fail-closed (#1090 §4.9 case 11): no entry, no row.
			return 0, fmt.Errorf("recordstore: WAL append failed for %s, row not written: %w", id, err)
		}
		rev, err := c.saveRow(ctx, id, kind, class, mime, data, lineage)
		if err != nil {
			// The WAL entry for nextRev already landed but the row never
			// will - append a compensating marker (best-effort) so
			// lastRevision skips this revision as a parent on the next
			// save, instead of every retry re-deriving the same phantom
			// nextRev and wedging forever behind a row that can't exist.
			appendAborted(ctx, c.ledgerStore, c.sessionID, lineage.TurnID, lineage.NodeID, id, nextRev, err.Error())
			return 0, err
		}
		if rev != nextRev {
			// Under the lock above, with aborted revisions skipped by
			// lastRevision, this is no longer explainable by a race or by a
			// prior failed save - the WAL and the store have genuinely
			// diverged. Fail closed rather than let a mismatched
			// revision-content pairing propagate silently.
			return 0, fmt.Errorf("recordstore: store assigned revision %d for %s, WAL expected %d", rev, id, nextRev)
		}
		return rev, nil
	}
	return c.saveRow(ctx, id, kind, class, mime, data, lineage)
}

func (c *Client) saveRow(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage) (int, error) {
	lineageJSON, err := json.Marshal(lineage)
	if err != nil {
		return 0, fmt.Errorf("recordstore: marshal lineage for %s: %w", id, err)
	}
	req := &artifact.SaveRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: id,
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mime}},
	}
	if ms, ok := c.svc.(metaSaver); ok {
		resp, err := ms.SaveWithMeta(ctx, req, kind, string(class), lineageJSON, lineage.TurnID)
		if err != nil {
			return 0, fmt.Errorf("recordstore: save %s: %w", id, err)
		}
		return int(resp.Version), nil
	}
	resp, err := c.svc.Save(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("recordstore: save %s: %w", id, err)
	}
	return int(resp.Version), nil
}

// SaveStructured validates doc against kind's registered validator, derives
// its id via the kind's Identity func (hint feeds identity for kinds whose
// instance comes from outside the content, e.g. a subject id; ignored by a
// content-hashed kind like finding), and saves it as a new JSON revision.
// Returns the derived id and the new revision.
func (c *Client) SaveStructured(ctx context.Context, kind string, doc any, hint string, lineage Lineage) (string, int, error) {
	spec, err := lookupKind(kind)
	if err != nil {
		return "", 0, err
	}
	if spec.Class != Structured {
		return "", 0, fmt.Errorf("recordstore: kind %q is not structured", kind)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", 0, fmt.Errorf("recordstore: marshal %s: %w", kind, err)
	}
	if spec.Validate != nil {
		if err := spec.Validate(raw); err != nil {
			return "", 0, fmt.Errorf("recordstore: %s failed validation: %w", kind, err)
		}
	}
	instance, err := spec.Identity(raw, hint)
	if err != nil {
		return "", 0, fmt.Errorf("recordstore: %s identity: %w", kind, err)
	}
	id := kind + idSep + instance
	rev, err := c.save(ctx, id, kind, Structured, "application/json", raw, lineage)
	return id, rev, err
}

// SaveBlob saves data (any mime, no validation beyond the size bound callers
// already enforce) as a new revision, deriving its id via the kind's
// Identity func the same way SaveStructured does.
func (c *Client) SaveBlob(ctx context.Context, kind string, data []byte, mime, hint string, lineage Lineage) (string, int, error) {
	spec, err := lookupKind(kind)
	if err != nil {
		return "", 0, err
	}
	if spec.Class != Blob {
		return "", 0, fmt.Errorf("recordstore: kind %q is not a blob", kind)
	}
	instance, err := spec.Identity(data, hint)
	if err != nil {
		return "", 0, fmt.Errorf("recordstore: %s identity: %w", kind, err)
	}
	id := kind + idSep + instance
	rev, err := c.save(ctx, id, kind, Blob, mime, data, lineage)
	return id, rev, err
}

// ponytail: no async Save*. The one caller (vetting's per-round write site)
// now calls SaveStructured/SaveBlob synchronously so it can capture the real
// assigned revision for the next round's ParentRevision, and so a node's own
// rounds for one id can never interleave (#1090 adversarial review finding
// #3). Add back a fire-and-forget wrapper if a caller with no revision-chain
// need shows up.
// IdentityFor computes what SaveStructured would derive as the id for doc,
// without saving - pure and synchronous, so a caller can know an id (e.g.
// for its own in-memory bookkeeping across rounds) before firing an async
// save. This is the only sanctioned way to obtain an id outside a save: the
// registry's Identity func is still the sole place identity logic lives.
func IdentityFor(kind string, doc any, hint string) (string, error) {
	spec, err := lookupKind(kind)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("recordstore: marshal %s: %w", kind, err)
	}
	instance, err := spec.Identity(raw, hint)
	if err != nil {
		return "", fmt.Errorf("recordstore: %s identity: %w", kind, err)
	}
	return kind + idSep + instance, nil
}

// versionsDesc returns id's saved revisions, newest first, nil if none.
func (c *Client) versionsDesc(ctx context.Context, id string) ([]int64, error) {
	vresp, err := c.svc.Versions(ctx, &artifact.VersionsRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: id,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("recordstore: versions %s: %w", id, err)
	}
	if vresp == nil {
		return nil, nil
	}
	versions := append([]int64(nil), vresp.Versions...)
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
	return versions, nil
}

// Latest returns the newest revision of id as raw bytes plus its revision.
// ok is false when no revision exists.
func (c *Client) Latest(ctx context.Context, id string) ([]byte, int, bool, error) {
	raw, _, _, rev, ok, err := c.LatestWithMeta(ctx, id)
	return raw, rev, ok, err
}

// LatestWithMeta is Latest, also returning mime and the row's lineage when
// the wrapped service supports it (zero Lineage otherwise - #1090 known
// ceiling for a non-Postgres backend, e.g. artifact.InMemoryService() in
// tests).
func (c *Client) LatestWithMeta(ctx context.Context, id string) ([]byte, string, Lineage, int, bool, error) {
	req := &artifact.LoadRequest{AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: id}
	var resp *artifact.LoadResponse
	var lineageJSON []byte
	var err error
	if ml, ok := c.svc.(metaLoader); ok {
		resp, _, _, lineageJSON, err = ml.LoadWithMeta(ctx, req)
	} else {
		resp, err = c.svc.Load(ctx, req)
	}
	if err != nil {
		if isNotFound(err) {
			return nil, "", Lineage{}, 0, false, nil
		}
		return nil, "", Lineage{}, 0, false, fmt.Errorf("recordstore: load %s: %w", id, err)
	}
	if resp == nil || resp.Part == nil || resp.Part.InlineData == nil {
		return nil, "", Lineage{}, 0, false, nil
	}
	var lineage Lineage
	_ = json.Unmarshal(lineageJSON, &lineage) // best-effort; zero value if absent/malformed
	versions, err := c.versionsDesc(ctx, id)
	rev := 0
	if err == nil && len(versions) > 0 {
		rev = int(versions[0])
	}
	return resp.Part.InlineData.Data, resp.Part.InlineData.MIMEType, lineage, rev, true, nil
}

// ArtifactSummary is one id's listing row (§4.4 list_artifacts).
type ArtifactSummary struct {
	ID       string
	Kind     string
	Revision int
	NodeID   string // lineage.node_id of the latest revision
}

// List returns every id in this chat whose kind matches kindFilter ("" =
// all), each with its latest revision and authoring node. Best-effort per
// id: an id that fails to load is skipped rather than failing the whole call.
func (c *Client) List(ctx context.Context, kindFilter string) ([]ArtifactSummary, error) {
	resp, err := c.svc.List(ctx, &artifact.ListRequest{AppName: c.appName, UserID: c.userID, SessionID: c.sessionID})
	if err != nil {
		return nil, fmt.Errorf("recordstore: list: %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]ArtifactSummary, 0, len(resp.FileNames))
	for _, id := range resp.FileNames {
		kind := KindOf(id)
		if kindFilter != "" && kind != kindFilter {
			continue
		}
		_, _, lineage, rev, ok, err := c.LatestWithMeta(ctx, id)
		if err != nil || !ok {
			continue
		}
		out = append(out, ArtifactSummary{ID: id, Kind: kind, Revision: rev, NodeID: lineage.NodeID})
	}
	return out, nil
}

// EditOp is one search/replace pair for Edit; Old must match the target
// content exactly once.
type EditOp struct {
	Old string
	New string
}

// EditConflict is returned when ops cannot be resolved against the current
// latest revision - the caller should show Content/Revision to the agent
// and let it retry with fresh edits.
type EditConflict struct {
	ID       string
	Revision int
	Content  []byte
}

func (e *EditConflict) Error() string {
	return fmt.Sprintf("recordstore: edit %s: no unique match against revision %d", e.ID, e.Revision)
}

// idLocks serializes revision allocation per (session, id) so two concurrent
// editors of the same artifact never race to the same next revision number -
// ponytail: process-local mutex, fine for quack's single-process server;
// upgrade to a DB-level lock only if a second server process joins.
var idLocks sync.Map // "app/user/session/id" -> *sync.Mutex

func (c *Client) lockFor(id string) *sync.Mutex {
	key := c.appName + "/" + c.userID + "/" + c.sessionID + "/" + id
	v, _ := idLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// applyEdits applies ops to content in order; each Old must appear exactly
// once in the content as of that point, else the whole batch is rejected
// (no partial writes) - ambiguous (0 or 2+ matches) is a failure.
func applyEdits(content []byte, ops []EditOp) ([]byte, error) {
	s := string(content)
	for _, op := range ops {
		n := strings.Count(s, op.Old)
		if n != 1 {
			return nil, fmt.Errorf("edit old-string matched %d times (want exactly 1): %q", n, op.Old)
		}
		s = strings.Replace(s, op.Old, op.New, 1)
	}
	return []byte(s), nil
}

// Edit applies ops to id's latest revision and writes N+1 (§4.4/§9). The
// merge is unconditional on baseRevision: whether the caller's base is
// current or stale, edits are always re-applied against whatever is latest
// right now, and succeed exactly when every Old still matches uniquely -
// that's what makes a stale-but-non-intersecting edit merge instead of
// failing. Structured content is re-validated before the write. Returns
// *EditConflict (with the current latest) on any match failure - never a
// partial write.
func (c *Client) Edit(ctx context.Context, id string, baseRevision int, ops []EditOp, lineage Lineage) (int, []byte, error) {
	mu := c.lockFor(id)
	mu.Lock()
	defer mu.Unlock()

	raw, mime, _, latestRev, ok, err := c.LatestWithMeta(ctx, id)
	if err != nil {
		return 0, nil, fmt.Errorf("recordstore: edit %s: %w", id, err)
	}
	if !ok {
		return 0, nil, fmt.Errorf("recordstore: edit %s: no revision exists", id)
	}
	if baseRevision < 0 {
		return 0, nil, fmt.Errorf("recordstore: edit %s: base_revision must be >= 0", id)
	}
	if baseRevision > latestRev {
		return 0, nil, fmt.Errorf("recordstore: edit %s: base_revision %d exceeds latest revision %d", id, baseRevision, latestRev)
	}
	if baseRevision != latestRev {
		// Worth observing in aggregate: how often edit_artifact merges against a
		// stale base rather than applying directly.
		slog.Debug("recordstore: edit merged against a newer revision than base_revision", "id", id, "base_revision", baseRevision, "latest_revision", latestRev)
	}
	merged, err := applyEdits(raw, ops)
	if err != nil {
		return 0, nil, &EditConflict{ID: id, Revision: latestRev, Content: raw}
	}
	kind := KindOf(id)
	spec, err := lookupKind(kind)
	if err != nil {
		return 0, nil, err
	}
	if spec.Class == Structured && spec.Validate != nil {
		if verr := spec.Validate(merged); verr != nil {
			return 0, nil, fmt.Errorf("recordstore: edit %s: result fails validation: %w", id, verr)
		}
	}
	lineage.ParentRevision = latestRev
	lineage.BaseRevision = baseRevision
	rev, err := c.save(ctx, id, kind, spec.Class, mime, merged, lineage)
	if err != nil {
		return 0, nil, err
	}
	return rev, merged, nil
}

// Kinds returns every registered structured kind's name and JSONSchema, for
// #1091's generated write_<kind> tools - one per structured kind.
func Kinds() []KindSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]KindSpec, 0, len(registry))
	for name, spec := range registry {
		if spec.Class != Structured {
			continue
		}
		spec.name = name
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ponytail: no delete/retention/list surface. Design V4.1 dropped
// delete-on-merged/closed and document retention outright - artifacts live
// until the chat itself is hard-deleted, so there is nothing here for those
// to call yet. Add back (KeepLastRevisions, DeleteAll, DeleteByKind, Names)
// when a real caller needs one - a chat hard-delete path, most likely.
