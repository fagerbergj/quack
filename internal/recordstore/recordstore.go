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

// idSep joins kind/instance (design's logical shape uses "/", but ADK's
// artifact.Service rejects "/" and "\" in FileName - validateFileName - so
// the physical record id substitutes ":"); an instance value may itself contain ":" (e.g. "pr:123"), which is safe because KindOf/id only ever split on the first separator.
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
// supplies (e.g. the chat's external subject identity) - never from a caller-composed string; a finding's identity ignores hint entirely and hashes fields inside content, so it comes out the same regardless of which node or hint produced it.
type IdentityFunc func(content []byte, hint string) (instance string, err error)

// KindSpec is one registered kind's shape: content class, schema version and
// JSON Schema text (for #1091's generated write_<kind> tools - "" for a blob
// kind, which has no schema), a validator (nil for a blob kind), and the identity function that derives its instance segment.
type KindSpec struct {
	Class         Class
	SchemaVersion int
	JSONSchema    string // #1091 tool generation input; the write_<kind> tool's input schema verbatim
	Validate      func(json.RawMessage) error
	Identity      IdentityFunc
	// RequiresHint declares that Identity fails without a non-empty hint
	// (the requireHint pattern) - callers that can't supply a real session
	// hint (e.g. write_artifact/write_<kind> for an arbitrary caller-chosen kind) use this to decide whether to pass one at all, rather than passing a hint unconditionally and corrupting a hint-optional kind's content-hash identity (#1108 finding 2).
	RequiresHint bool
	// AgentWritable gates the generic write_<kind> MCP/ADK tool generators
	// (#1091) - false for a gate-only kind (judge_round, delivery_record) so
	// a worker can't forge a verdict/delivery record into the gate's WAL.
	AgentWritable bool

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
// code_review, finding, document, pr_body, text, bytes. Panics on a duplicate registration, a spec missing Identity (a wiring bug - every kind must derive its own id), or a JSONSchema that doesn't parse: the write_<kind> tool generators (MCP and ADK) both trust this schema is valid and previously skipped the tool silently on either surface when it wasn't, so failing here means both surfaces fail the same way, loudly, at startup (#1108 finding 3).
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
		// An empty schema on a structured kind survives startup but then makes
		// the two write_<kind> generators diverge - MCP Warns-and-skips, ADK
		// hard-fails the whole run (#1108 L1) - so reject it here instead.
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
// NodeID is provenance only (the node that authored this revision), never part of the artifact's id.
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
// artifact.Service without them - artifact.InMemoryService(), used by most tests - still saves/loads fine; kind/class/lineage just don't persist.
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
	// ledgerStore: the WAL's fail-closed AppendIntent path (#1090 §4.9/#1100);
	// nil = no WAL; Save* behaves exactly as before #1100. Set only via
	// WithLedger, by a caller that has already restricted it to a transactional (postgres) backend - see vetting.Config.Ledger's doc.
	ledgerStore ledger.LedgerStore
}

// New scopes a client to one session over svc (the artifact.Service the
// concrete store wraps, e.g. *store.TurnAwareService).
func New(svc artifact.Service, appName, userID, sessionID string) *Client {
	return &Client{svc: svc, appName: appName, userID: userID, sessionID: sessionID}
}

// WithLedger arms the fail-closed WAL path on c and returns c. store should
// already be filtered to a transactional backend by the caller (see
// vetting.Config.Ledger) - recordstore itself doesn't inspect the backend kind, it just trusts a non-nil store to make AppendIntent atomic.
func (c *Client) WithLedger(store ledger.LedgerStore) *Client {
	c.ledgerStore = store
	return c
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

// maxSaveRetries bounds save/Edit's retry on ledger.ErrStaleParent - a real
// conflict resolves next attempt; this only guards a wedged id (saveAt's doc).
const maxSaveRetries = 20

// idempotencyKey derives the store-level dedup key for id/data (#1144 P4),
// replacing the old read-then-compare "identical to latest" check.
// Deliberately content-only, NOT parentRev-qualified: a writer's crash-then-retry must collide with its OWN original intent no matter how far the tip has moved since, so claimAndSave's content-mismatch check (#1237) can still catch a different writer having adopted the same orphaned slot. A save whose bytes match an OLDER, non-tip revision falls through to revertKey instead (finding 2).
func idempotencyKey(id string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// revertKey is claimAndSave's fallback claim key when a dup hit's matched
// revision isn't the one the caller expected resolved (finding 2: a
// deliberate revert, not a retry of the same attempt) - qualified by parentRev so the retried claim can't collide with that same stale entry.
func revertKey(id string, parentRev int, data []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", parentRev)
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// errStaleContentMatch: claimAndSave's dup hit matched identical content at
// a revision other than expectResolvedAt - not this attempt's own intent,
// so the caller must retry under a fresh key rather than treat it as done.
var errStaleContentMatch = errors.New("recordstore: idempotency key matched a stale, non-tip revision")

// adoptAfterAttempts bounds how many plain retries saveAtOrAdopt insists on
// before it will treat a still-conflicting parent as an orphan (saveAt's doc
// below). A live concurrent writer for the SAME id needs only one AppendIntent + one saveRow to finish; giving up several real round trips of headroom first means "still conflicting" is overwhelmingly a genuine crash, not a writer that simply hasn't reached saveRow yet - the #1100 stress test (20 goroutines racing one brand-new id) is exactly the case an immediate check-and-adopt gets wrong.
const adoptAfterAttempts = 3

// retryBackoff is the delay before save/Edit's next ErrStaleParent retry -
// small, growing, jittered, so maxSaveRetries's 20 attempts under real
// contention are not a tight spin against Postgres.
func retryBackoff(attempt int) time.Duration {
	d := time.Duration(attempt+1) * 3 * time.Millisecond
	if d > 30*time.Millisecond {
		d = 30 * time.Millisecond
	}
	return d + time.Duration(rand.IntN(5))*time.Millisecond
}

// idLocks serializes read-latest through row-write per (chat,id): the ledger's
// unique index only orders WAL claims, while artifact.Service numbers revisions
// independently, so unlocked writers can end up with claimed != stored (#1144 P4). ponytail: process-local, a second replica needs a Postgres advisory lock.
var idLocks sync.Map // "chatID\x00id" -> *sync.Mutex

func (c *Client) lockFor(id string) *sync.Mutex {
	v, _ := idLocks.LoadOrStore(c.sessionID+"\x00"+id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// save picks id's current latest revision as the parent and retries on
// ledger.ErrStaleParent for a cross-process conflict; a same-process
// conflict for the same id can't happen at all - lockFor serializes it.
func (c *Client) save(ctx context.Context, id, kind string, class Class, mime string, data []byte, lineage Lineage) (int, error) {
	mu := c.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	for attempt := 0; ; attempt++ {
		// No ledger: nothing arbitrates writers anyway, so keep the caller's
		// own tracked ParentRevision as before #1144 P4.
		parentRev := lineage.ParentRevision
		if c.ledgerStore != nil {
			versions, err := c.versionsDesc(ctx, id)
			if err != nil {
				return 0, fmt.Errorf("recordstore: read latest revision for %s: %w", id, err)
			}
			parentRev = 0
			if len(versions) > 0 {
				parentRev = int(versions[0])
				// Saving exactly what's already at the tip is a genuine
				// no-op: report it with no new revision and no WAL entry.
				// Content matching an OLDER, non-latest revision (a revert) falls through to saveAtOrAdopt and mints its own new one: this tip comparison is what makes only a same-as-tip save collapse; idempotencyKey itself is content-only (finding 2).
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

// saveAtOrAdopt tries saveAt at parentRev; on ledger.ErrStaleParent, once
// attempt reaches adoptAfterAttempts it checks whether the intent that
// already claimed parentRev is an orphan - a crash or transient backend error left its row unwritten (#1144 P4 follow-up: checking "does the row exist" costs nothing extra a normal save wasn't already going to do, replacing byte-duplication into the ledger). If parentRev+1 has no row yet, this save ADOPTS that slot: writes the row with its OWN data at that exact revision, completing the orphaned intent instead of failing forever. If the slot fills in between the check and the write (a genuine concurrent writer that was just slow), the mismatch falls through to a normal ErrStaleParent retry.
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

// saveAt writes data as parentRev+1, first claiming that parent in the
// ledger (#1144 P4). Tries the content-only key first so a crash-retry
// fails closed against a foreign adoption (#1237); only a stale, non-tip match (finding 2's revert case) falls back to a parentRev-qualified key.
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

// claimAndSave appends the ledger intent under key, claiming parentRev+1,
// and completes it. expectResolvedAt is the revision a dup hit must match
// to count as THIS attempt's own no-op (current tip for the content-only key; parentRev+1 for revertKey's fallback claim) - a dup hit anywhere else with matching bytes is a stale match (errStaleContentMatch); with mismatching bytes it's a foreign writer's adoption (#1237, fail closed).
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
		// The idempotency key is committed by AppendIntent BEFORE saveRow, so
		// a duplicate hit alone does NOT prove the row was ever written - the
		// original save may have crashed between the two (#1237 review: this used to report success with no row). Verify first; an orphaned duplicate is completed the same way saveAtOrAdopt completes any other orphan, never reported as a no-op without a row.
		if existing, ok, lerr := c.LoadVersion(ctx, id, p.Revision); lerr == nil && ok {
			// The row can exist WITHOUT matching our content: a different
			// writer may have adopted this same orphaned slot first (#1237
			// review). Comparing bytes, not just presence, is what makes this actually "identical content already recorded" rather than a silent handoff of someone else's data under our name.
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
		// Fail-closed (#1090 §4.9 case 11), including ledger.ErrStaleParent:
		// no entry, no row.
		return 0, err
	}
	rev, err := c.saveRow(ctx, id, kind, class, mime, data, lineage)
	if err != nil {
		return 0, err
	}
	if rev != nextRev {
		// The ledger claimed nextRev but the store assigned something else -
		// with the parent claim now exclusive at the store level, this can
		// only mean the two have genuinely diverged: fail closed rather than let a mismatched revision-content pairing propagate silently.
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

// SaveStructured validates doc against kind's registered validator, derives
// its id via the kind's Identity func (hint feeds identity for kinds whose
// instance comes from outside the content, e.g. a subject id; ignored by a content-hashed kind like finding), and saves it as a new JSON revision, returning the derived id and the new revision.
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
// assigned revision for the next round's ParentRevision, and so a node's own rounds for one id can never interleave (#1090 adversarial review finding #3); add back a fire-and-forget wrapper if a caller with no revision-chain need shows up.
// IdentityFor computes what SaveStructured would derive as the id for doc,
// without saving - pure and synchronous, so a caller can know an id (e.g.
// for its own in-memory bookkeeping across rounds) before firing an async save; the only sanctioned way to obtain an id outside a save, with the registry's Identity func still the sole place identity logic lives.
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
// ceiling for a non-Postgres backend, e.g. artifact.InMemoryService() in tests).
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

// Versions returns id's saved revision numbers, newest first, nil if none -
// the public counterpart of versionsDesc, for a caller (delivery_record's
// history read, #1093) that needs every revision of one id, not just Latest.
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

// stringLeaf is one JSON string value found while walking a document, along
// with the raw byte span of its quoted literal (including the quotes) - the
// span applyStructuredEdits splices a re-encoded replacement into.
type stringLeaf struct {
	start, end int
	value      string
}

// stringLeaves walks content's JSON token stream and returns every string
// that is a value (array element or object field value), never an object
// key - a match against a key, or one that would only exist by concatenating two adjacent leaves, is invisible here and correctly counts as no match.
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

// applyStructuredEdits is applyEdits for Structured content: each Old is
// matched against decoded JSON string values (so a New containing a raw
// newline, quote, or backslash is encoded correctly) rather than raw bytes. Old must occur exactly once across every leaf's decoded value combined - 0 or 2+ is ambiguous, same failure as applyEdits; only the matched leaf is re-encoded and spliced back in, so everything else (key order, spacing) is untouched and a no-op edit is byte-identical.
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

// Edit applies ops to id's latest revision and writes N+1 (§4.4/§9). The
// merge is unconditional on baseRevision: edits are always re-applied against
// whatever is latest right now, and succeed exactly when every Old still matches uniquely - that's what makes a stale-but-non-intersecting edit merge instead of failing; structured content is re-validated before the write, and any match failure returns *EditConflict (with the current latest), never a partial write. Holds the same per-id lock save() does, so an Edit and a gate save can never interleave their read-latest and write; a cross-process conflict still retries on ledger.ErrStaleParent: reread the new latest and reapply ops, same as a stale baseRevision.
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
	// Structured edits target decoded field text - a raw byte search/replace on
	// the serialized JSON breaks the moment New has a newline, quote, or
	// backslash. Blob has no JSON structure to speak of, so it keeps the byte path.
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
	lineage.BaseRevision = baseRevision
	rev, err := c.saveAtOrAdopt(ctx, id, kind, spec.Class, mime, merged, lineage, latestRev, attempt)
	if err != nil {
		return 0, nil, err
	}
	return rev, merged, nil
}

// Kinds returns every registered structured kind's name and JSONSchema, for
// #1091's generated write_<kind> tools - one per structured kind.
func Kinds() []KindSpec { return KindsForClass(Structured) }

// KindsForClass returns every registered spec of class, name populated,
// sorted by name - the only enumerator that can see Blob kinds (Kinds() only
// ever returned Structured, so write_artifact's kind list rendered empty; #1108 finding 2 regression).
func KindsForClass(class Class) []KindSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]KindSpec, 0, len(registry))
	for name, spec := range registry {
		if spec.Class != class {
			continue
		}
		spec.name = name
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ArtifactKindNames returns the sorted names of every registered blob-class
// kind - the closed set a node's `artifact` field may select, since
// SaveBlob only accepts a registered kind (#1128).
func ArtifactKindNames() []string {
	specs := KindsForClass(Blob)
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name()
	}
	return names
}

// ValidateArtifactKind rejects an artifact selector that isn't one of
// ArtifactKindNames() - the shared guard for #1128, used at plan-build time
// (dag, planner-authored nodes) and at config-bind time (config, operator-authored workflow nodes) so both routes to SaveBlob agree.
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
