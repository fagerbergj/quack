// Package artifactsrc resolves quack's named prompt artifacts - agent system
// prompts, rubrics, memory guidance and the fragments under config/prompts -
// from a configured Source, falling back to the shipped file (internal/bundledir).
package artifactsrc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/fagerbergj/quack/internal/bundledir"
)

// StaticSource is Artifact.Source for a shipped file (disk, then embedded).
const StaticSource = "static"

// DefaultTTL bounds how long a resolved artifact is reused; a prompt edited in
// the store (or on disk) takes effect on the first round past it.
const DefaultTTL = 60 * time.Second

// Artifact is one resolved prompt artifact. Config carries the model/effort
// binding a stored version may declare (unused until #1421); Source and
// VersionID are the provenance stamped onto the round's llm.call entries.
type Artifact struct {
	Name      string
	Body      string
	Config    map[string]any
	Source    string
	VersionID string
}

// Source is a store artifacts can be resolved from ahead of the shipped file.
// Get reports (_, false, nil) for a name the store does not have; Seed
// publishes the shipped version of a name the store is missing.
type Source interface {
	Get(ctx context.Context, name string) (Artifact, bool, error)
	Seed(ctx context.Context, name string, static Artifact) error
}

// Resolver resolves names through a Source with a TTL cache, falling back to
// the shipped file. The nil *Resolver is the static-only configuration (no
// prompts: block), so every consumer can take one unconditionally.
type Resolver struct {
	src  Source
	name string
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	art Artifact
	at  time.Time
}

// New builds a Resolver over src, whose resolved artifacts are stamped with
// srcName (the stores: entry). A nil src, or ttl <= 0, take the defaults.
func New(srcName string, src Source, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if srcName == "" {
		srcName = StaticSource
	}
	return &Resolver{src: src, name: srcName, ttl: ttl, cache: map[string]cached{}}
}

// Resolve returns name's artifact: the Source's version when it has one, else
// the shipped file. A Source that errors falls back to the file and warns.
func (r *Resolver) Resolve(ctx context.Context, name string) (Artifact, error) {
	if r == nil {
		return Static(name)
	}
	now := time.Now()
	r.mu.Lock()
	if c, ok := r.cache[name]; ok && now.Sub(c.at) < r.ttl {
		r.mu.Unlock()
		return c.art, nil
	}
	r.mu.Unlock()

	art, err := r.fetch(ctx, name)
	if err != nil {
		return Artifact{}, err
	}
	// Timed after the fetch, not before: a slow store must not spend the TTL it
	// was meant to start.
	r.mu.Lock()
	r.cache[name] = cached{art: art, at: time.Now()}
	r.mu.Unlock()
	return art, nil
}

func (r *Resolver) fetch(ctx context.Context, name string) (Artifact, error) {
	if r.src == nil {
		return Static(name)
	}
	art, ok, err := r.src.Get(ctx, name)
	switch {
	case err != nil:
		slog.Warn("prompt source failed; using the shipped artifact",
			"component", "artifacts", "store", r.name, "artifact", name, "err", err)
	case ok:
		art.Name = name
		if art.Source == "" {
			art.Source = r.name
		}
		return art, nil
	}
	return Static(name)
}

// ResolveUsable is Resolve with a content check: a blank body from a store is a
// prompt someone emptied by accident, not an edit, so it falls back to the
// shipped file rather than running the model with no instruction.
func (r *Resolver) ResolveUsable(ctx context.Context, name string) (Artifact, error) {
	art, err := r.Resolve(ctx, name)
	if err != nil {
		return Artifact{}, err
	}
	if strings.TrimSpace(art.Body) != "" {
		return art, nil
	}
	if art.Source == StaticSource {
		return Artifact{}, fmt.Errorf("artifacts: shipped %s is empty", name)
	}
	slog.Warn("resolved prompt is blank; using the shipped file",
		"component", "artifacts", "artifact", name, "source", art.Source, "version", art.VersionID)
	return Static(name)
}

// Pinned is the artifact one node is running on. A round refreshes it at its
// start and nothing re-resolves until the next one, so the prompt the model
// sees and the version the ledger records can never disagree mid-round.
type Pinned struct {
	res  *Resolver
	name string
	mu   sync.Mutex
	art  Artifact
}

// NewPinned pins boot's artifact under name; an empty name never refreshes
// (a bundle outside agents/ has no artifact to resolve).
func NewPinned(res *Resolver, name string, boot Artifact) *Pinned {
	return &Pinned{res: res, name: name, art: boot}
}

// Get is the pinned artifact - a field read, cheap enough for every model call.
func (p *Pinned) Get() Artifact {
	if p == nil {
		return Artifact{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.art
}

// Refresh re-resolves at a round's start and returns what the round will use.
// An unresolvable or unusable version keeps the pinned one: a round already
// under way is worth more than the edit that would have replaced its prompt.
func (p *Pinned) Refresh(ctx context.Context) Artifact {
	if p == nil {
		return Artifact{}
	}
	if p.name == "" {
		return p.Get()
	}
	art, err := p.res.ResolveUsable(ctx, p.name)
	if err != nil {
		slog.Warn("prompt unresolved; keeping the pinned version",
			"component", "artifacts", "artifact", p.name, "err", err)
		return p.Get()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.art = art
	return art
}

// TemplateCache holds one parsed template per artifact version - the same
// prompt is re-rendered every round and parsing is the expensive half.
type TemplateCache struct {
	mu  sync.Mutex
	key string
	t   *template.Template
}

func (c *TemplateCache) parse(art Artifact) (*template.Template, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t != nil && c.key == art.Source+"/"+art.VersionID {
		return c.t, nil
	}
	t, err := template.New(art.Name).Parse(art.Body)
	if err != nil {
		return nil, err
	}
	c.key, c.t = art.Source+"/"+art.VersionID, t
	return t, nil
}

// Render resolves name, parses it and hands the template to render. A stored
// version that will not parse or render falls back to the shipped file with one
// warning - a bad prompt edit must degrade, never disable the gate that reads it.
func Render(ctx context.Context, res *Resolver, cache *TemplateCache, name string, render func(*template.Template) error) (Artifact, error) {
	art, err := res.ResolveUsable(ctx, name)
	if err != nil {
		return Artifact{}, err
	}
	rerr := renderWith(cache, art, render)
	if rerr == nil {
		return art, nil
	}
	if art.Source == StaticSource {
		return Artifact{}, fmt.Errorf("artifacts: %s: %w", name, rerr)
	}
	slog.Warn("resolved prompt will not render; using the shipped file",
		"component", "artifacts", "artifact", name, "source", art.Source, "version", art.VersionID, "err", rerr)
	shipped, err := Static(name)
	if err != nil {
		return Artifact{}, err
	}
	if err := renderWith(cache, shipped, render); err != nil {
		return Artifact{}, fmt.Errorf("artifacts: shipped %s: %w", name, err)
	}
	return shipped, nil
}

func renderWith(cache *TemplateCache, art Artifact, render func(*template.Template) error) error {
	t, err := cache.parse(art)
	if err != nil {
		return err
	}
	return render(t)
}

// Static reads name's shipped file: disk in cwd first, then the embedded copy.
// VersionID is the body's content hash, so an edited file is a new version.
func Static(name string) (Artifact, error) {
	p, ok := StaticPath(name)
	if !ok {
		return Artifact{}, fmt.Errorf("artifacts: unknown artifact %q", name)
	}
	raw, err := bundledir.ReadFile(p)
	if err != nil {
		return Artifact{}, fmt.Errorf("artifacts: read %s (%s): %w", name, p, err)
	}
	return FileArtifact(name, raw), nil
}

// FileArtifact stamps raw as a static artifact under name. Exported for the
// files that have no artifact name (a bundle outside agents/): they still need
// a content-hash version id, or the ledger records none.
func FileArtifact(name string, raw []byte) Artifact {
	sum := sha256.Sum256(raw)
	return Artifact{Name: name, Body: string(raw), Source: StaticSource, VersionID: hex.EncodeToString(sum[:])[:12]}
}

// registry maps every shipped artifact name to its file. Derived from the
// shipped tree once, never hand-listed - see scan.
var registry = sync.OnceValue(scan)

// Names lists every artifact the shipped files define, for seeding a Source.
func Names() []string {
	reg := registry()
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	return names
}

// StaticPath returns the shipped file backing name.
func StaticPath(name string) (string, bool) {
	p, ok := registry()[name]
	return p, ok
}

// BundleName is the artifact name of kind ("system", "rubric" or "memory") for
// the agent bundle at dir, or "" when dir is not a shipped agents/<x> bundle.
func BundleName(kind, dir string) string {
	base, agent := path.Split(path.Clean(dir))
	if path.Clean(base) != "agents" || agent == "" {
		return ""
	}
	name := kind + "/" + agent
	if _, ok := StaticPath(name); !ok {
		return ""
	}
	return name
}

// ReadBundleFile reads file from the agent bundle at dir through the resolver when the bundle is
// a shipped agents/<x> (kind is "system", "rubric" or "memory"), and straight off disk-then-embedded
// otherwise - a bundle outside agents/, or one missing that file, has no artifact name to resolve.
func ReadBundleFile(ctx context.Context, res *Resolver, kind, dir, file string) ([]byte, error) {
	if name := BundleName(kind, dir); name != "" {
		art, err := res.Resolve(ctx, name)
		if err != nil {
			return nil, err
		}
		return []byte(art.Body), nil
	}
	return bundledir.ReadFile(bundledir.PathJoin(dir, file))
}

// scan derives the name registry from the shipped tree: each agents/<x>/ gives system/<x> plus
// rubric/<x> and memory/<x> when present, config/rubric.md and config/constitution.md give
// rubric/global and rubric/constitution, and each config/prompts/<n>.md gives system/<n>.
func scan() map[string]string {
	reg := map[string]string{}
	add := func(name, p string) bool {
		if _, err := bundledir.ReadFile(p); err != nil {
			return false
		}
		reg[name] = p
		return true
	}
	for _, a := range bundledir.UnionDirNames("agents", ".") {
		if !add("system/"+a, path.Join("agents", a, "prompt.md")) {
			continue // not a bundle dir (a stray file under agents/)
		}
		add("rubric/"+a, path.Join("agents", a, "rubric.yaml"))
		add("memory/"+a, path.Join("agents", a, "memory.md"))
	}
	add("rubric/global", "config/rubric.md")
	add("rubric/constitution", "config/constitution.md")
	for _, f := range bundledir.UnionDirNames("config", "prompts") {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		add("system/"+strings.TrimSuffix(f, ".md"), path.Join("config", "prompts", f))
	}
	// A shipped name that resolves nowhere means a broken image or a bind-mount
	// that shadowed it; the resolver would fail the round with "unknown artifact".
	for _, n := range []string{"system/judge", "system/compaction", "system/compaction.summary", "system/acp.environment", "rubric/global"} {
		if _, ok := reg[n]; !ok {
			slog.Error("shipped prompt artifact is missing", "component", "artifacts", "artifact", n)
		}
	}
	return reg
}
