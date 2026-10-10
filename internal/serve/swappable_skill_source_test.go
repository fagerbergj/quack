package serve

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// fakeSkillSource tags every response with a name so a test can tell which
// backing source answered.
type fakeSkillSource struct{ tag string }

func (f fakeSkillSource) ListFrontmatters(context.Context) ([]*skill.Frontmatter, error) {
	return []*skill.Frontmatter{{Name: f.tag}}, nil
}
func (f fakeSkillSource) LoadFrontmatter(context.Context, string) (*skill.Frontmatter, error) {
	return &skill.Frontmatter{Name: f.tag}, nil
}
func (f fakeSkillSource) LoadInstructions(context.Context, string) (string, error) {
	return f.tag, nil
}
func (f fakeSkillSource) LoadResource(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(nil), errors.New(f.tag)
}
func (f fakeSkillSource) ListResources(context.Context, string, string) ([]string, error) {
	return []string{f.tag}, nil
}

// TestSwappableSkillSourceDelegatesAndSwaps: every method reaches the current backing source and Swap
// changes it for all at once (rebuildSkills depends on this).
func TestSwappableSkillSourceDelegatesAndSwaps(t *testing.T) {
	s := newSwappableSkillSource(fakeSkillSource{"v1"})
	ctx := context.Background()

	fms, _ := s.ListFrontmatters(ctx)
	if len(fms) != 1 || fms[0].Name != "v1" {
		t.Fatalf("ListFrontmatters = %v, want v1", fms)
	}
	fm, _ := s.LoadFrontmatter(ctx, "x")
	if fm.Name != "v1" {
		t.Fatalf("LoadFrontmatter = %v, want v1", fm)
	}
	if ins, _ := s.LoadInstructions(ctx, "x"); ins != "v1" {
		t.Fatalf("LoadInstructions = %q, want v1", ins)
	}
	if _, err := s.LoadResource(ctx, "x", "y"); err == nil || err.Error() != "v1" {
		t.Fatalf("LoadResource err = %v, want v1", err)
	}
	if names, _ := s.ListResources(ctx, "x", "y"); len(names) != 1 || names[0] != "v1" {
		t.Fatalf("ListResources = %v, want v1", names)
	}

	s.Swap(fakeSkillSource{"v2"})
	fms, _ = s.ListFrontmatters(ctx)
	if len(fms) != 1 || fms[0].Name != "v2" {
		t.Fatalf("ListFrontmatters after Swap = %v, want v2", fms)
	}
}

// TestSwappableSkillSourceConcurrentReadsDuringSwap: readers looping List/LoadFrontmatter never
// see a torn value while Swap runs concurrently.
func TestSwappableSkillSourceConcurrentReadsDuringSwap(t *testing.T) {
	s := newSwappableSkillSource(fakeSkillSource{"v1"})
	ctx := context.Background()
	done := make(chan struct{})

	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = s.ListFrontmatters(ctx)
				_, _ = s.LoadFrontmatter(ctx, "x")
			}
		}
	}()

	for i := 0; i < 100; i++ {
		s.Swap(fakeSkillSource{"v2"})
	}
	close(done)
	readerWG.Wait()
}
