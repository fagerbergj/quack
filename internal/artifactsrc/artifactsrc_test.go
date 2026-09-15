package artifactsrc

import (
	"context"
	"errors"
	"slices"
	"testing"
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
