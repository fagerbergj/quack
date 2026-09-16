// Package artifactsrc resolves quack's named prompt artifacts - agent system
// prompts, rubrics, memory guidance and the fragments under config/prompts -
// from a configured Source, falling back to the shipped file (internal/bundledir).
package artifactsrc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
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
	r.mu.Lock()
	r.cache[name] = cached{art: art, at: now}
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
	sum := sha256.Sum256(raw)
	return Artifact{Name: name, Body: string(raw), Source: StaticSource, VersionID: hex.EncodeToString(sum[:])[:12]}, nil
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
	add := func(name, p string) {
		if _, err := bundledir.ReadFile(p); err == nil {
			reg[name] = p
		}
	}
	if des, err := fs.ReadDir(bundledir.SubFS("agents"), "."); err == nil {
		for _, de := range des {
			if !de.IsDir() {
				continue
			}
			a := de.Name()
			add("system/"+a, path.Join("agents", a, "prompt.md"))
			add("rubric/"+a, path.Join("agents", a, "rubric.yaml"))
			add("memory/"+a, path.Join("agents", a, "memory.md"))
		}
	}
	add("rubric/global", "config/rubric.md")
	add("rubric/constitution", "config/constitution.md")
	if des, err := fs.ReadDir(bundledir.SubFS("config"), "prompts"); err == nil {
		for _, de := range des {
			if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
				continue
			}
			n := strings.TrimSuffix(de.Name(), ".md")
			add("system/"+n, path.Join("config", "prompts", de.Name()))
		}
	}
	return reg
}
