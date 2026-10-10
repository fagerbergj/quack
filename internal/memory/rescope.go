package memory

import (
	"context"
	"fmt"
)

// ChatRepoResolver resolves a chat's GitHub origin to RepoIdentity's format (e.g. "github.com/acme/games"),
// ok=false if it has none. internal/serve wires it, since this package can't import internal/store.
type ChatRepoResolver func(ctx context.Context, chatID string) (repoKey string, ok bool)

// RescopeRepoStat is one target repo's rescope tally.
type RescopeRepoStat struct {
	Count    int
	Examples []string // a few memory content previews, for --dry-run sanity checking
}

// RescopeResult is Rescope's report: per-repo tallies plus points with no provenance chat_id.
type RescopeResult struct {
	ByRepo              map[string]*RescopeRepoStat
	SkippedNoProvenance int
}

const rescopeExamplesPerRepo = 3

type rescopeMove struct {
	id, srcBucket, dstBucket string
}

// Rescope moves live role:* memories whose provenance chat resolves to a repo into that repo's bucket.
// Writes happen after the scan: moving while paging role:* would skip rows, breaking dry-run == apply.
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

// rescopeOne tallies one memory's repo and returns its move when apply is set; nil when it has no
// provenance chat_id (counted as skipped) or no GitHub origin.
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
