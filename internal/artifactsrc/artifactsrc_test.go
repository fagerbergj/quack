package artifactsrc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"
)

// stubSource records its calls and returns whatever the test set up.
type stubSource struct {
	art   Artifact
	ok    bool
	err   error
	calls int
}

func (s *stubSource) Get(context.Context, string) (Artifact, bool, error) {
	s.calls++
	return s.art, s.ok, s.err
}

func (s *stubSource) Seed(context.Context, string, Artifact) error { return nil }

func TestStaticResolvesShippedFile(t *testing.T) {
	art, err := (*Resolver)(nil).Resolve(context.Background(), "system/code-reviewer")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if art.Source != StaticSource {
		t.Errorf("source = %q, want %q", art.Source, StaticSource)
	}
	if len(art.VersionID) != 12 {
		t.Errorf("version id = %q, want 12 hex chars of the body's sha256", art.VersionID)
	}
	if art.Body == "" {
		t.Error("body is empty")
	}
	// Same bytes, same version: the id is the content hash, nothing else.
	again, err := Static("system/code-reviewer")
	if err != nil || again.VersionID != art.VersionID {
		t.Errorf("version id not stable: %q vs %q (err %v)", again.VersionID, art.VersionID, err)
	}
}

func TestResolveUnknownName(t *testing.T) {
	if _, err := New("", nil, 0).Resolve(context.Background(), "system/nope"); err == nil {
		t.Fatal("resolving an unknown name must error, not fall through to an empty prompt")
	}
}

func TestResolvePrefersSourceAndCachesForTTL(t *testing.T) {
	src := &stubSource{art: Artifact{Body: "from the store", VersionID: "7"}, ok: true}
	r := New("langfuse", src, time.Minute)
	for i := range 3 {
		art, err := r.Resolve(context.Background(), "system/judge")
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if art.Body != "from the store" || art.Source != "langfuse" || art.VersionID != "7" {
			t.Fatalf("resolve %d = %+v, want the store's artifact stamped with the store name", i, art)
		}
	}
	if src.calls != 1 {
		t.Errorf("source hit %d times within the TTL, want 1", src.calls)
	}
}

func TestResolveRefetchesPastTTL(t *testing.T) {
	src := &stubSource{art: Artifact{Body: "v1"}, ok: true}
	r := New("langfuse", src, time.Nanosecond)
	ctx := context.Background()
	if _, err := r.Resolve(ctx, "system/judge"); err != nil {
		t.Fatal(err)
	}
	src.art = Artifact{Body: "v2"}
	art, err := r.Resolve(ctx, "system/judge")
	if err != nil {
		t.Fatal(err)
	}
	if art.Body != "v2" {
		t.Errorf("body = %q, want the re-fetched v2 once the TTL expired", art.Body)
	}
}

func TestResolveFallsBackWhenSourceLacksTheName(t *testing.T) {
	src := &stubSource{ok: false}
	art, err := New("langfuse", src, time.Minute).Resolve(context.Background(), "system/judge")
	if err != nil {
		t.Fatalf("a name the store lacks must fall back silently, got %v", err)
	}
	if art.Source != StaticSource || art.Body == "" {
		t.Errorf("got %+v, want the shipped file", art)
	}
}

func TestResolveFallsBackWhenSourceErrors(t *testing.T) {
	src := &stubSource{err: errors.New("langfuse unreachable")}
	art, err := New("langfuse", src, time.Minute).Resolve(context.Background(), "system/judge")
	if err != nil {
		t.Fatalf("a failing store must not fail the round, got %v", err)
	}
	if art.Source != StaticSource || art.Body == "" {
		t.Errorf("got %+v, want the shipped file", art)
	}
	static, _ := Static("system/judge")
	if art.VersionID != static.VersionID {
		t.Errorf("version id = %q, want the static %q", art.VersionID, static.VersionID)
	}
}

// TestNamesDerivedFromShippedFiles: the registry is scanned, never hand-listed,
// so a new agent bundle or config/prompts file needs no code change.
func TestNamesDerivedFromShippedFiles(t *testing.T) {
	names := Names()
	for _, want := range []string{
		"system/code-reviewer", "memory/code-reviewer", "rubric/code-reviewer",
		"rubric/global", "rubric/constitution",
		"system/judge", "system/compaction", "system/compaction.summary", "system/acp.environment",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() is missing %q", want)
		}
		if _, ok := StaticPath(want); !ok {
			t.Errorf("StaticPath(%q) not found", want)
		}
	}
	if p, _ := StaticPath("system/code-reviewer"); p != "agents/code-reviewer/prompt.md" {
		t.Errorf("system/code-reviewer maps to %q", p)
	}
	if p, _ := StaticPath("rubric/global"); p != "config/rubric.md" {
		t.Errorf("rubric/global maps to %q", p)
	}
}

// TestResolveUsableRejectsBlankBody: a store handing back an empty prompt has
// lost it, not edited it - the shipped file must win rather than the model
// running with no instruction.
func TestResolveUsableRejectsBlankBody(t *testing.T) {
	src := &stubSource{art: Artifact{Body: "   \n\t "}, ok: true}
	art, err := New("langfuse", src, time.Minute).ResolveUsable(context.Background(), "system/judge")
	if err != nil {
		t.Fatalf("a blank stored prompt must fall back, got %v", err)
	}
	shipped, _ := Static("system/judge")
	if art.Source != StaticSource || art.VersionID != shipped.VersionID {
		t.Errorf("got %+v, want the shipped %s@%s", art, StaticSource, shipped.VersionID)
	}
}

// TestPinnedRefreshKeepsUsable: a round under way is worth more than the edit
// that would have replaced its prompt.
func TestPinnedRefreshKeepsUsable(t *testing.T) {
	boot := Artifact{Body: "boot", Source: StaticSource, VersionID: "bootver"}
	src := &stubSource{art: Artifact{Body: "stored", VersionID: "v9"}, ok: true}
	p := NewPinned(New("langfuse", src, time.Nanosecond), "system/judge", boot)
	if got := p.Get(); got.VersionID != "bootver" {
		t.Fatalf("Get before any refresh = %+v, want boot", got)
	}
	if got := p.Refresh(context.Background()); got.Body != "stored" || p.Get().Body != "stored" {
		t.Fatalf("refresh = %+v, want the stored version pinned", got)
	}
	src.art = Artifact{Body: ""} // blank is unusable: the shipped file wins
	if got := p.Refresh(context.Background()); strings.TrimSpace(got.Body) == "" {
		t.Error("refresh pinned a blank prompt")
	}
	// An unnamed holder (a bundle outside agents/) never re-resolves.
	if got := NewPinned(nil, "", boot).Refresh(context.Background()); got.VersionID != "bootver" {
		t.Errorf("unnamed holder refreshed to %+v, want boot", got)
	}
}

// TestRenderFallsBackOnBadTemplate: a stored prompt that will not parse or
// render degrades to the shipped file instead of erroring out its caller -
// for system/judge, erroring would disable the trust gate deployment-wide.
func TestRenderFallsBackOnBadTemplate(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"unparseable", "{{if .Broken}}no end"},
		{"missing block", "no define blocks here"},
	} {
		src := &stubSource{art: Artifact{Body: c.body, VersionID: "bad"}, ok: true}
		var cache TemplateCache
		var out strings.Builder
		art, err := Render(context.Background(), New("langfuse", src, time.Minute), &cache, "system/judge",
			func(tm *template.Template) error {
				out.Reset()
				return tm.ExecuteTemplate(&out, "head", nil)
			})
		if err != nil {
			t.Errorf("%s: Render = %v, want a silent fall back to the shipped file", c.name, err)
			continue
		}
		if art.Source != StaticSource || out.Len() == 0 {
			t.Errorf("%s: got %+v with %d rendered bytes, want the shipped file", c.name, art, out.Len())
		}
	}
}

// TestTemplateCacheReparsesOnNewVersion: keyed on the version, so an edited
// prompt is never served from a stale parse.
func TestTemplateCacheReparsesOnNewVersion(t *testing.T) {
	var cache TemplateCache
	first, err := cache.parse(Artifact{Name: "x", Body: "A", Source: StaticSource, VersionID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if same, _ := cache.parse(Artifact{Name: "x", Body: "A", Source: StaticSource, VersionID: "1"}); first != same {
		t.Error("same version re-parsed; the cache is not keyed on the version")
	}
	if next, _ := cache.parse(Artifact{Name: "x", Body: "B", Source: StaticSource, VersionID: "2"}); next == first {
		t.Error("new version served a stale parse")
	}
}

// mapSource answers Get by exact name from a fixed map; a miss is (_, false, nil).
type mapSource map[string]Artifact

func (m mapSource) Get(_ context.Context, name string) (Artifact, bool, error) {
	art, ok := m[name]
	return art, ok, nil
}
func (mapSource) Seed(context.Context, string, Artifact) error { return nil }

// TestChainSource_PinnedThenStore covers serve's promptSourceFor chain (#1424 item 6):
// a pinned override wins for the name it pins; any other name falls through to the store.
func TestChainSource_PinnedThenStore(t *testing.T) {
	pin := mapSource{"system/code-reviewer": {Name: "system/code-reviewer", Body: "pinned"}}
	store := mapSource{
		"system/code-reviewer": {Name: "system/code-reviewer", Body: "store version"},
		"system/synthesizer":   {Name: "system/synthesizer", Body: "unpinned"},
	}
	chain := Chain(pin, store)

	art, ok, err := chain.Get(context.Background(), "system/code-reviewer")
	if err != nil || !ok || art.Body != "pinned" {
		t.Fatalf("pinned name = %+v ok=%v err=%v, want the pin's body", art, ok, err)
	}
	art, ok, err = chain.Get(context.Background(), "system/synthesizer")
	if err != nil || !ok || art.Body != "unpinned" {
		t.Fatalf("unpinned name = %+v ok=%v err=%v, want the store's body", art, ok, err)
	}
	if _, ok, err := chain.Get(context.Background(), "system/nowhere"); err != nil || ok {
		t.Fatalf("name in neither source: ok=%v err=%v, want false,nil", ok, err)
	}
}

func TestChainSource_SeedDelegatesToLast(t *testing.T) {
	last := &stubSource{}
	chain := Chain(&stubSource{}, last)
	if err := chain.Seed(context.Background(), "x", Artifact{}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleName(t *testing.T) {
	for _, c := range []struct{ kind, dir, want string }{
		{"system", "agents/code-reviewer", "system/code-reviewer"},
		{"memory", "agents/code-reviewer/", "memory/code-reviewer"},
		{"system", "/opt/custom-bundle", ""}, // outside agents/, no artifact name
		{"memory", "agents/advisor", ""},     // shipped bundle with no memory.md
	} {
		if got := BundleName(c.kind, c.dir); got != c.want {
			t.Errorf("BundleName(%q, %q) = %q, want %q", c.kind, c.dir, got, c.want)
		}
	}
}
