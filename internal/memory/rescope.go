package memory

import (
	"context"
	"fmt"
)

// ChatRepoResolver resolves a chat's GitHub origin (owner/repo) to the same identity format
// RepoIdentity/NormalizeRepoURL produce (e.g. "github.com/acme/games"). ok=false
// when the chat has no GitHub origin. internal/memory can't import internal/store (dependency direction), so the server bootstrap wires a concrete resolver over the chat store.
type ChatRepoResolver func(ctx context.Context, chatID string) (repoKey string, ok bool)

// RescopeRepoStat is one target repo's rescope tally.
type RescopeRepoStat struct {
	Count    int
	Examples []string // a few memory content previews, for --dry-run sanity checking
}

// RescopeResult is Rescope's full report: per-repo tallies plus points that
// couldn't be resolved at all (no provenance chat_id - #875 predates it).
type RescopeResult struct {
	ByRepo              map[string]*RescopeRepoStat
	SkippedNoProvenance int
}

const rescopeExamplesPerRepo = 3

type rescopeMove struct {
	id, srcBucket, dstBucket string
}

// Rescope moves every live role:* memory whose provenance chat resolves to a GitHub repo into
// that repo's bucket (#1262: worktree-per-node made RepoKey return "" for years of memories,
// all landing in role:coding/research instead of repo:<x>). apply=false only tallies; apply=true also writes the bucket change and a memory_ops audit row per point. The scan is tally-only; every write happens in a second pass over the collected moves. role:* shrinks as points are moved out of it, so writing WHILE paginating that same bucket would skip the page tail that shifted under the offset - the apply tally must equal the dry-run tally by construction, not by luck of page size vs. eligible count.
func (s *Store) Rescope(ctx context.Context, resolve ChatRepoResolver, apply bool) (RescopeResult, error) {
	result := RescopeResult{ByRepo: map[string]*RescopeRepoStat{}}
	var moves []rescopeMove
	for _, bucket := range []string{prefixed(bucketRole, RoleCoding), prefixed(bucketRole, RoleResearch)} {
		offset := 0
		for {
			mems, total, err := s.List(ctx, []string{bucket}, offset, DefaultListLimit, false, "")
			if err != nil {
				return RescopeResult{}, fmt.Errorf("memory: rescope list %q: %w", bucket, err)
			}
			for _, m := range mems {
				if mv := rescopeOne(ctx, m, resolve, apply, &result, bucket); mv != nil {
					moves = append(moves, *mv)
				}
			}
			offset += len(mems)
			if offset >= total || len(mems) == 0 {
				break
			}
		}
	}
	if result.SkippedNoProvenance > 0 {
		s.log.Info("rescope: points without chat provenance not moved", "count", result.SkippedNoProvenance)
	}
	for _, mv := range moves {
		if err := s.idx.updateBucket(ctx, mv.id, mv.dstBucket); err != nil {
			return RescopeResult{}, fmt.Errorf("memory: rescope update %q: %w", mv.id, err)
		}
		s.logOp(ctx, mv.id, OpUpdate, ActorRescope, "rescope: "+mv.srcBucket+" -> "+mv.dstBucket)
	}
	return result, nil
}

// rescopeOne is Rescope's per-memory step: tally the resolved repo into the
// report and record the move when apply is set. nil = not a move: no
// provenance chat_id (counted as skipped) or the chat has no GitHub origin.
func rescopeOne(ctx context.Context, m Memory, resolve ChatRepoResolver, apply bool, result *RescopeResult, srcBucket string) *rescopeMove {
	if m.ChatID == "" {
		result.SkippedNoProvenance++
		return nil
	}
	repoKey, ok := resolve(ctx, m.ChatID)
	if !ok || repoKey == "" {
		return nil
	}
	stat := result.ByRepo[repoKey]
	if stat == nil {
		stat = &RescopeRepoStat{}
		result.ByRepo[repoKey] = stat
	}
	stat.Count++
	if len(stat.Examples) < rescopeExamplesPerRepo {
		stat.Examples = append(stat.Examples, preview(m.Content))
	}
	if !apply {
		return nil
	}
	return &rescopeMove{id: m.ID, srcBucket: srcBucket, dstBucket: prefixed(bucketRepo, repoKey)}
}
