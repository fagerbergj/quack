// Package recordstore is a typed record store over the ADK artifact.Service. A record's kind/instance id is
// derived from the saved content by the kind's registered Identity func, never composed by a caller.
package recordstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
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

// idSep joins kind/instance: artifact.Service rejects "/" in FileName. An instance may itself contain ":"
// (e.g. "pr:123"); that is safe because ids are only ever split on the first separator.
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

// IdentityFunc derives a kind's instance segment from the marshaled content and an optional caller hint
// (e.g. the chat's subject identity), never from a caller-composed string.
type IdentityFunc func(content []byte, hint string) (instance string, err error)

// KindSpec is one registered kind's shape. JSONSchema feeds the generated write_<kind> tools ("" and a
// nil Validate for a blob kind).
type KindSpec struct {
	Class         Class
	SchemaVersion int
	JSONSchema    string // the write_<kind> tool's input schema verbatim
	Validate      func(json.RawMessage) error
	Identity      IdentityFunc
	// RequiresHint: Identity fails without a non-empty hint. Callers check it rather than always passing a
	// hint, which would corrupt a hint-optional kind's content-hash identity.
	RequiresHint bool
	// AgentWritable gates the generic write_<kind> tools; false for gate-only kinds (judge_round,
	// delivery_record) so a worker can't forge a verdict or delivery record.
	AgentWritable bool
	// System excludes a Blob kind from KindsForClass(Blob), so from
	// write_artifact/MCP and the plan-level kind selector - agent-forgeable evidence risk (e.g. web_page).
	System bool

	name string // set only by Kinds(); not part of the registered spec
}

// Name is the registered kind name (populated on values returned by Kinds()).
func (k KindSpec) Name() string { return k.name }

var (
	registryMu sync.RWMutex
	registry   = map[string]KindSpec{}
)

// Register declares kind's shape; call once per kind from package init. It panics on a duplicate, a missing
// Identity, or an unparseable JSONSchema, so both write_<kind> tool surfaces fail loudly at startup.
func Register(kind string, spec KindSpec) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[kind]; ok {
		panic("recordstore: kind " + kind + " already registered")
	}
	if spec.Identity == nil {
		panic("recordstore: kind " + kind + " registered with no Identity func")
	}
	if spec.Class == Structured && spec.JSONSchema == "" {
		// An empty schema survives startup but makes the MCP and ADK write_<kind> generators diverge
		// (one skips, the other fails the run).
		panic("recordstore: kind " + kind + " registered as structured without a JSONSchema")
	}
	if spec.JSONSchema != "" {
		var schema jsonschema.Schema
		if err := json.Unmarshal([]byte(spec.JSONSchema), &schema); err != nil {
			panic("recordstore: kind " + kind + " registered with invalid JSONSchema: " + err.Error())
		}
	}
	registry[kind] = spec
}

// SpecFor returns kind's registered spec, so a generic write path can check RequiresHint for a
// caller-chosen kind.
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

// Lineage is the per-revision provenance stamped on the store row, not inside the bytes, so blob kinds carry
// it too. NodeID is provenance only, never part of the artifact's id.
type Lineage struct {
	NodeID         string `json:"node_id"`
	Round          int    `json:"round"`
	ParentRevision int    `json:"parent_revision"`
	// BaseRevision is the revision the editor last read: advisory, for observability only. The merge always
	// targets whatever is actually latest.
	BaseRevision      int       `json:"base_revision"`
	TriggerAnnotation string    `json:"trigger_annotation,omitempty"`
	HeadSHA           string    `json:"head_sha,omitempty"`
	SavedAt           time.Time `json:"saved_at"`
	Author            string    `json:"author"`
	// SourceURL: the external URL this revision's content was fetched from,
	// if any (e.g. web_page) - content itself stays a pure copy of the page.
	SourceURL string `json:"source_url,omitempty"`
	// TurnID targets the store row's turn_id column, so it is excluded from the lineage JSON.
	TurnID string `json:"-"`
}

// metaSaver/metaLoader are implemented by *store.TurnAwareService, checked structurally to avoid importing
// internal/store. Without them saves still work, but kind/class/lineage don't persist.
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
	// ledgerStore is the fail-closed WAL path; nil means no WAL. Set only via WithLedger, by a caller that
	// already restricted it to a transactional (postgres) backend.
	ledgerStore ledger.LedgerStore
	// schemas: per-kind JSON Schemas an SDK extension declared (WithSchemas);
	// nil = no enforcement, Save*/Edit behave exactly as before this existed.
	schemas SchemaRegistry
}

// SchemaRegistry is the subset of artifactschema.Registry a Client needs -
// declared here so artifactschema can import recordstore without a cycle.
type SchemaRegistry interface {
	Validate(kind string, content []byte) []string
	Schema(kind string) json.RawMessage
}

// New scopes a client to one session over svc (the artifact.Service the
// concrete store wraps, e.g. *store.TurnAwareService).
func New(svc artifact.Service, appName, userID, sessionID string) *Client {
	return &Client{svc: svc, appName: appName, userID: userID, sessionID: sessionID}
}

// WithLedger arms the fail-closed WAL path on c. The caller must already restrict store to a transactional
// backend; recordstore trusts a non-nil store to make AppendIntent atomic.
func (c *Client) WithLedger(store ledger.LedgerStore) *Client {
	c.ledgerStore = store
	return c
}

// WithSchemas arms schema enforcement on c and returns c: a Save*/Edit whose
// kind has a registered schema must satisfy it or the write is refused.
func (c *Client) WithSchemas(reg SchemaRegistry) *Client {
	c.schemas = reg
	return c
}

// SchemaViolation is returned by Save*/Edit when kind's registered artifact
// schema rejects the content - the write never reaches the backing store.
type SchemaViolation struct {
	Kind       string
	Violations []string
	Schema     json.RawMessage
}

func (e *SchemaViolation) Error() string {
	return fmt.Sprintf("recordstore: %s failed its schema (%d violation(s))", e.Kind, len(e.Violations))
}

// checkSchema is a no-op unless WithSchemas was called and has kind registered.
func (c *Client) checkSchema(kind string, content []byte) error {
	if c.schemas == nil {
		return nil
	}
	violations := c.schemas.Validate(kind, content)
	if violations == nil {
		return nil
	}
	return &SchemaViolation{Kind: kind, Violations: violations, Schema: c.schemas.Schema(kind)}
}

// artifactRevisionPayload: bytes_ref is the store row's key, never the bytes, so a large blob stays one small
// WAL entry.
type artifactRevisionPayload struct {
	ID             string  `json:"id"`
	Revision       int     `json:"revision"`
	ParentRevision int     `json:"parent_revision"`
	Kind           string  `json:"kind"`
	Class          Class   `json:"class"`
	Lineage        Lineage `json:"lineage"`
	BytesRef       string  `json:"bytes_ref"`
}

// maxSaveRetries bounds save/Edit's retry on ledger.ErrStaleParent - a real
// conflict resolves next attempt; this only guards a wedged id (saveAt's doc).
const maxSaveRetries = 20

// idempotencyKey is content-only, not parentRev-qualified: a crash-then-retry must collide with its own
// intent however far the tip has moved. A match on an older, non-tip revision falls through to revertKey.
func idempotencyKey(id string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// revertKey is the fallback claim key for a deliberate revert (dup hit at an unexpected revision), qualified
// by parentRev so the retried claim can't collide with that stale entry.
func revertKey(id string, parentRev int, data []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", parentRev)
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// errStaleContentMatch: a dup hit matched identical content at a revision other than expectResolvedAt, so
// the caller must retry under a fresh key rather than treat it as done.
var errStaleContentMatch = errors.New("recordstore: idempotency key matched a stale, non-tip revision")

// adoptAfterAttempts plain retries come before treating a conflicting parent as an orphan: a live writer
// needs one round trip to finish, so adopting immediately would steal a slow writer's slot.
const adoptAfterAttempts = 3

// retryBackoff is small, growing and jittered so maxSaveRetries under contention isn't a tight spin.
func retryBackoff(attempt int) time.Duration {
	d := time.Duration(attempt+1) * 3 * time.Millisecond
	if d > 30*time.Millisecond {
		d = 30 * time.Millisecond
	}
	return d + time.Duration(rand.IntN(5))*time.Millisecond
}

// idLocks serializes read-latest through row-write per (chat,id): artifact.Service numbers revisions
// independently of the ledger. ponytail: process-local, a second replica needs a Postgres advisory lock.
var idLocks sync.Map // "chatID\x00id" -> *sync.Mutex

func (c *Client) lockFor(id string) *sync.Mutex {
	v, _ := idLocks.LoadOrStore(c.sessionID+"\x00"+id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

var keyLocks sync.Map // "chatID\x00key" -> *sync.Mutex

// LockKey serializes a caller's read-modify-write of several records under key
// within this chat. Separate from the per-id save lock, so Save* may run inside it.
func (c *Client) LockKey(key string) (unlock func()) {
	v, _ := keyLocks.LoadOrStore(c.sessionID+"\x00"+key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// save uses id's latest revision as the parent and retries on ledger.ErrStaleParent for a cross-process
// conflict; lockFor rules out same-process conflicts.
func (c *Client) save(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage) (int, error) {
	mu := c.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	for attempt := 0; ; attempt++ {
		// No ledger: nothing arbitrates writers anyway, so keep the caller's own tracked ParentRevision.
		parentRev := lineage.ParentRevision
		if c.ledgerStore != nil {
			versions, err := c.versionsDesc(ctx, id)
			if err != nil {
				return 0, fmt.Errorf("recordstore: read latest revision for %s: %w", id, err)
			}
			parentRev = 0
			if len(versions) > 0 {
				parentRev = int(versions[0])
				// Saving exactly the tip is a no-op: no new revision, no WAL entry. Matching an older
				// revision (a revert) falls through and mints a new one.
				if latest, ok, lerr := c.LoadVersion(ctx, id, parentRev); lerr == nil && ok && bytes.Equal(latest, data) {
					return parentRev, nil
				}
			}
		}
		rev, err := c.saveAtOrAdopt(ctx, id, kind, class, mime, data, lineage, parentRev, attempt)
		if errors.Is(err, ledger.ErrStaleParent) && attempt < maxSaveRetries {
			time.Sleep(retryBackoff(attempt))
			continue
		}
		return rev, err
	}
}

// saveAtOrAdopt retries saveAt; past adoptAfterAttempts, if parentRev+1 still has no row its claim is an
// orphan (crashed writer) and this save adopts that slot with its own data. A slot filled meanwhile retries.
func (c *Client) saveAtOrAdopt(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage, parentRev, attempt int) (int, error) {
	rev, err := c.saveAt(ctx, id, kind, class, mime, data, lineage, parentRev)
	if !errors.Is(err, ledger.ErrStaleParent) || c.ledgerStore == nil || attempt < adoptAfterAttempts {
		return rev, err
	}
	if _, ok, lerr := c.LoadVersion(ctx, id, parentRev+1); lerr != nil || ok {
		return rev, err // row already exists (or the check itself failed) - not an orphan, retry normally
	}
	lineage.ParentRevision = parentRev
	adopted, aerr := c.saveRow(ctx, id, kind, class, mime, data, lineage)
	if aerr != nil || adopted != parentRev+1 {
		return rev, err // lost the race or the write failed - fall back to a normal retry
	}
	return adopted, nil
}

// saveAt writes data as parentRev+1 after claiming that parent in the ledger. The content-only key comes
// first so a crash-retry fails closed against a foreign adoption; only a stale match falls back to revertKey.
func (c *Client) saveAt(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage, parentRev int) (int, error) {
	lineage.ParentRevision = parentRev
	if c.ledgerStore == nil {
		return c.saveRow(ctx, id, kind, class, mime, data, lineage)
	}
	rev, err := c.claimAndSave(ctx, id, kind, class, mime, data, lineage, parentRev, idempotencyKey(id, data), parentRev)
	if errors.Is(err, errStaleContentMatch) {
		return c.claimAndSave(ctx, id, kind, class, mime, data, lineage, parentRev, revertKey(id, parentRev, data), parentRev+1)
	}
	return rev, err
}

// claimAndSave claims parentRev+1 under key and writes the row. A dup hit is this attempt's own no-op only
// at expectResolvedAt with matching bytes; elsewhere it's stale, and mismatched bytes are a foreign adoption.
func (c *Client) claimAndSave(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage, parentRev int, key string, expectResolvedAt int) (int, error) {
	nextRev := parentRev + 1
	payload, err := json.Marshal(artifactRevisionPayload{ID: id, Revision: nextRev, ParentRevision: parentRev, Kind: kind, Class: class, Lineage: lineage, BytesRef: id})
	if err != nil {
		return 0, fmt.Errorf("recordstore: marshal artifact.revision payload for %s: %w", id, err)
	}
	_, err = c.ledgerStore.AppendIntent(ctx, ledger.Entry{
		ChatID: c.sessionID, TurnID: lineage.TurnID, NodeID: lineage.NodeID,
		Kind: ledger.KindArtifactRevision, Key: id, At: time.Now().UTC(), Payload: payload,
		IdempotencyKey: key,
	})
	var dup *ledger.DuplicateIntentError
	if errors.As(err, &dup) {
		var p artifactRevisionPayload
		if jerr := json.Unmarshal(dup.Existing.Payload, &p); jerr != nil {
			return 0, fmt.Errorf("recordstore: duplicate intent for %s had an unparseable payload: %w", id, jerr)
		}
		// AppendIntent commits the key before saveRow, so a duplicate hit doesn't prove the row exists:
		// verify it, and complete an orphan like saveAtOrAdopt does.
		if existing, ok, lerr := c.LoadVersion(ctx, id, p.Revision); lerr == nil && ok {
			// A different writer may have adopted this orphaned slot first; compare bytes, not just presence,
			// so someone else's data is never reported as ours.
			if !bytes.Equal(existing, data) {
				return 0, fmt.Errorf("recordstore: duplicate intent for %s matched idempotency key but revision %d's content differs - a different writer already adopted this slot", id, p.Revision)
			}
			if p.Revision != expectResolvedAt {
				return 0, errStaleContentMatch
			}
			slog.Info("recordstore: save skipped, identical content already recorded", "component", "recordstore", "id", id, "kind", kind, "revision", p.Revision)
			return p.Revision, nil
		}
		lineage.ParentRevision = p.ParentRevision
		adopted, aerr := c.saveRow(ctx, id, kind, class, mime, data, lineage)
		if aerr != nil {
			return 0, aerr
		}
		if adopted != p.Revision {
			return 0, fmt.Errorf("recordstore: store assigned revision %d completing duplicate intent for %s, WAL expected %d", adopted, id, p.Revision)
		}
		return adopted, nil
	}
	if err != nil {
		// Fail closed, including ledger.ErrStaleParent: no entry, no row.
		return 0, err
	}
	rev, err := c.saveRow(ctx, id, kind, class, mime, data, lineage)
	if err != nil {
		return 0, err
	}
	if rev != nextRev {
		// The store assigned a different revision than the ledger claimed: they have diverged, so fail
		// closed rather than pair a revision with the wrong content.
		return 0, fmt.Errorf("recordstore: store assigned revision %d for %s, WAL expected %d", rev, id, nextRev)
	}
	return rev, nil
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

// SaveStructured validates doc, derives its id via the kind's Identity func (hint feeds kinds whose instance
// comes from outside the content), and saves it as a new JSON revision.
func (c *Client) SaveStructured(ctx context.Context, kind string, doc any, hint string, lineage Lineage) (string, int, error) {
	return c.saveStructured(ctx, kind, doc, hint, lineage, true)
}

// ResaveStructured is SaveStructured minus the kind's validator. System rewrites only, of a
// record the validator already accepted whose rules depend on live state (dag_node's roster).
func (c *Client) ResaveStructured(ctx context.Context, kind string, doc any, hint string, lineage Lineage) (string, int, error) {
	return c.saveStructured(ctx, kind, doc, hint, lineage, false)
}

func (c *Client) saveStructured(ctx context.Context, kind string, doc any, hint string, lineage Lineage, validate bool) (string, int, error) {
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
	if validate && spec.Validate != nil {
		if err := spec.Validate(raw); err != nil {
			return "", 0, fmt.Errorf("recordstore: %s failed validation: %w", kind, err)
		}
	}
	if err := c.checkSchema(kind, raw); err != nil {
		return "", 0, err
	}
	instance, err := spec.Identity(raw, hint)
	if err != nil {
		return "", 0, fmt.Errorf("recordstore: %s identity: %w", kind, err)
	}
	id := kind + idSep + instance
	rev, err := c.save(ctx, id, kind, Structured, "application/json", raw, lineage)
	return id, rev, err
}

// SaveBlob saves data of any mime as a new revision, deriving its id like SaveStructured.
func (c *Client) SaveBlob(ctx context.Context, kind string, data []byte, mime, hint string, lineage Lineage) (string, int, error) {
	spec, err := lookupKind(kind)
	if err != nil {
		return "", 0, err
	}
	if spec.Class != Blob {
		return "", 0, fmt.Errorf("recordstore: kind %q is not a blob", kind)
	}
	if err := c.checkSchema(kind, data); err != nil {
		return "", 0, err
	}
	instance, err := spec.Identity(data, hint)
	if err != nil {
		return "", 0, fmt.Errorf("recordstore: %s identity: %w", kind, err)
	}
	id := kind + idSep + instance
	rev, err := c.save(ctx, id, kind, Blob, mime, data, lineage)
	return id, rev, err
}

// ponytail: no async Save*; callers need the assigned revision for the next ParentRevision.
// Add a fire-and-forget wrapper if a caller without a revision chain shows up.

// IdentityFor computes the id SaveStructured would derive for doc, without saving: the only sanctioned way
// to obtain an id outside a save.
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

// LatestWithMeta is Latest plus mime and lineage when the wrapped service supports it (zero Lineage
// otherwise, e.g. artifact.InMemoryService()).
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

// LoadVersionWithMeta is LoadVersion plus that revision's own lineage (zero where LatestWithMeta's is).
func (c *Client) LoadVersionWithMeta(ctx context.Context, id string, version int) ([]byte, Lineage, bool, error) {
	req := &artifact.LoadRequest{AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: id, Version: int64(version)}
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
			return nil, Lineage{}, false, nil
		}
		return nil, Lineage{}, false, fmt.Errorf("recordstore: load %s@%d: %w", id, version, err)
	}
	if resp == nil || resp.Part == nil || resp.Part.InlineData == nil {
		return nil, Lineage{}, false, nil
	}
	var lineage Lineage
	_ = json.Unmarshal(lineageJSON, &lineage) // best-effort; zero value if absent/malformed
	return resp.Part.InlineData.Data, lineage, true, nil
}

// Versions returns id's saved revision numbers, newest first, nil if none.
func (c *Client) Versions(ctx context.Context, id string) ([]int, error) {
	vs, err := c.versionsDesc(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = int(v)
	}
	return out, nil
}

// LoadVersion returns one specific revision of id's content, ok=false if
// that revision doesn't exist.
func (c *Client) LoadVersion(ctx context.Context, id string, version int) ([]byte, bool, error) {
	resp, err := c.svc.Load(ctx, &artifact.LoadRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: id, Version: int64(version),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("recordstore: load %s@%d: %w", id, version, err)
	}
	if resp == nil || resp.Part == nil || resp.Part.InlineData == nil {
		return nil, false, nil
	}
	return resp.Part.InlineData.Data, true, nil
}

// ArtifactSummary is one id's list_artifacts row.
type ArtifactSummary struct {
	ID       string
	Kind     string
	Revision int
	NodeID   string    // lineage.node_id of the latest revision
	SavedAt  time.Time // lineage.saved_at of the latest revision; zero if unavailable (e.g. artifact.InMemoryService())
}

// List returns every id whose kind matches kindFilter ("" = all) with its latest revision and authoring node.
// An id that fails to load is skipped rather than failing the call.
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
		out = append(out, ArtifactSummary{ID: id, Kind: kind, Revision: rev, NodeID: lineage.NodeID, SavedAt: lineage.SavedAt})
	}
	return out, nil
}

// EditOp is one search/replace pair for Edit; Old must match the target
// content exactly once.
type EditOp struct {
	Old string
	New string
}

// EditConflict is returned when ops don't resolve against the current latest; the caller shows
// Content/Revision to the agent so it can retry with fresh edits.
type EditConflict struct {
	ID       string
	Revision int
	Content  []byte
}

func (e *EditConflict) Error() string {
	return fmt.Sprintf("recordstore: edit %s: no unique match against revision %d", e.ID, e.Revision)
}

// applyEdits applies ops in order; each Old must match exactly once at that point, else the whole batch is
// rejected (no partial writes).
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

// stringLeaf is one JSON string value plus the byte span of its quoted literal, where a replacement is spliced.
type stringLeaf struct {
	start, end int
	value      string
}

// stringLeaves returns every JSON string value, never an object key, so a match on a key or across two
// adjacent leaves correctly counts as no match.
func stringLeaves(content []byte) ([]stringLeaf, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	type frame struct{ isObject, keyNext bool }
	var stack []frame
	var leaves []stringLeaf
	prevEnd := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		top := func() *frame {
			if len(stack) == 0 {
				return nil
			}
			return &stack[len(stack)-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{isObject: true, keyNext: true})
			case '[':
				stack = append(stack, frame{isObject: false})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if f := top(); f != nil && f.isObject {
					f.keyNext = true
				}
			}
		case string:
			if f := top(); f != nil && f.isObject && f.keyNext {
				f.keyNext = false // this string is a key, not a candidate leaf
				break
			}
			start := bytes.IndexByte(content[prevEnd:], '"') + prevEnd
			leaves = append(leaves, stringLeaf{start: start, end: end, value: t})
			if f := top(); f != nil && f.isObject {
				f.keyNext = true
			}
		default: // number, bool, nil - always a value, never a key
			if f := top(); f != nil && f.isObject {
				f.keyNext = true
			}
		}
		prevEnd = end
	}
	return leaves, nil
}

// applyStructuredEdits matches each Old against decoded JSON string values, so a New with a newline, quote or
// backslash encodes correctly. Only the matched leaf is re-encoded, so key order and spacing are untouched.
func applyStructuredEdits(content []byte, ops []EditOp) ([]byte, error) {
	for _, op := range ops {
		leaves, err := stringLeaves(content)
		if err != nil {
			return nil, fmt.Errorf("edit: invalid JSON: %w", err)
		}
		total := 0
		var match *stringLeaf
		for i := range leaves {
			if n := strings.Count(leaves[i].value, op.Old); n > 0 {
				total += n
				match = &leaves[i]
			}
		}
		if total != 1 {
			return nil, fmt.Errorf("edit old-string matched %d times (want exactly 1): %q", total, op.Old)
		}
		newValue, err := json.Marshal(strings.Replace(match.value, op.Old, op.New, 1))
		if err != nil {
			return nil, fmt.Errorf("edit: encoding replacement: %w", err)
		}
		out := make([]byte, 0, len(content)-(match.end-match.start)+len(newValue))
		out = append(out, content[:match.start]...)
		out = append(out, newValue...)
		out = append(out, content[match.end:]...)
		content = out
	}
	return content, nil
}

// Edit re-applies ops to id's current latest, whatever baseRevision was, and writes N+1; any match failure
// returns *EditConflict, never a partial write. It holds save's per-id lock and retries on ErrStaleParent.
func (c *Client) Edit(ctx context.Context, id string, baseRevision int, ops []EditOp, lineage Lineage) (int, []byte, error) {
	mu := c.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	for attempt := 0; ; attempt++ {
		rev, merged, err := c.tryEdit(ctx, id, baseRevision, ops, lineage, attempt)
		if errors.Is(err, ledger.ErrStaleParent) && attempt < maxSaveRetries {
			time.Sleep(retryBackoff(attempt))
			continue
		}
		return rev, merged, err
	}
}

func (c *Client) tryEdit(ctx context.Context, id string, baseRevision int, ops []EditOp, lineage Lineage, attempt int) (int, []byte, error) {
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
	kind := KindOf(id)
	spec, err := lookupKind(kind)
	if err != nil {
		return 0, nil, err
	}
	if spec.System {
		return 0, nil, fmt.Errorf("recordstore: edit %s: kind %q is not editable directly", id, kind)
	}
	// Structured edits target decoded field text: raw byte replace on JSON breaks on newlines, quotes or
	// backslashes. Blob keeps the byte path.
	var merged []byte
	if spec.Class == Structured {
		merged, err = applyStructuredEdits(raw, ops)
	} else {
		merged, err = applyEdits(raw, ops)
	}
	if err != nil {
		return 0, nil, &EditConflict{ID: id, Revision: latestRev, Content: raw}
	}
	if spec.Class == Structured && spec.Validate != nil {
		if verr := spec.Validate(merged); verr != nil {
			return 0, nil, fmt.Errorf("recordstore: edit %s: result fails validation: %w", id, verr)
		}
	}
	if err := c.checkSchema(kind, merged); err != nil {
		return 0, nil, err
	}
	lineage.BaseRevision = baseRevision
	rev, err := c.saveAtOrAdopt(ctx, id, kind, spec.Class, mime, merged, lineage, latestRev, attempt)
	if err != nil {
		return 0, nil, err
	}
	return rev, merged, nil
}

// Kinds returns every registered structured kind's name and JSONSchema, for the generated write_<kind> tools.
func Kinds() []KindSpec { return KindsForClass(Structured) }

// KindsForClass returns every registered spec of class, name populated, sorted by name.
func KindsForClass(class Class) []KindSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]KindSpec, 0, len(registry))
	for name, spec := range registry {
		if spec.Class != class || spec.System {
			continue
		}
		spec.name = name
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ArtifactKindNames returns the sorted blob-class kinds: the closed set a node's `artifact` field may select.
func ArtifactKindNames() []string {
	specs := KindsForClass(Blob)
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name()
	}
	return names
}

// ValidateArtifactKind rejects a selector outside ArtifactKindNames(); shared by plan-build (dag) and
// config-bind (config) so both routes to SaveBlob agree.
func ValidateArtifactKind(kind string) error {
	for _, name := range ArtifactKindNames() {
		if name == kind {
			return nil
		}
	}
	return fmt.Errorf("artifact %q is not a registered kind; valid kinds: %s", kind, strings.Join(ArtifactKindNames(), ", "))
}

// ponytail: no delete/retention/list surface. Design V4.1 dropped
// delete-on-merged/closed and document retention outright - artifacts live
// until the chat itself is hard-deleted, so there is nothing here for those to call yet; add back (KeepLastRevisions, DeleteAll, DeleteByKind, Names) when a real caller needs one - a chat hard-delete path, most likely.
