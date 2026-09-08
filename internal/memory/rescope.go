package memory

import (
	"context"
	"fmt"
)

// ChatRepoResolver resolves a chat's GitHub origin (owner/repo) to the same
// identity format RepoIdentity/NormalizeRepoURL produce (e.g.
// "github.com/acme/games"). ok=false when the chat has no GitHub origin.
// internal/memory can't import internal/store (dependency direction), so the
// server bootstrap wires a concrete resolver over the chat store.
type ChatRepoResolver func(ctx context.Context, chatID string) (repoKey string, ok bool)

// RescopeRepoStat is one target repo's rescope tally.
type RescopeRepoStat struct {
	Count    int
	Examples []string // a few memory content previews, for --dry-run sanity checking
}

const rescopeExamplesPerRepo = 3

// Rescope moves every live role:* memory whose provenance chat resolves to a
// GitHub repo into that repo's bucket (#1262: worktree-per-node made RepoKey
// return "" for years of memories, all landing in role:coding/research
// instead of repo:<x>). apply=false only tallies; apply=true also writes the
// bucket change and a memory_ops audit row per point.
func (s *Store) Rescope(ctx context.Context, resolve ChatRepoResolver, apply bool) (map[string]*RescopeRepoStat, error) {
	byRepo := map[string]*RescopeRepoStat{}
	for _, bucket := range []string{prefixed(bucketRole, RoleCoding), prefixed(bucketRole, RoleResearch)} {
		offset := 0
		for {
			mems, total, err := s.List(ctx, []string{bucket}, offset, DefaultListLimit, false)
			if err != nil {
				return nil, fmt.Errorf("memory: rescope list %q: %w", bucket, err)
			}
			for _, m := range mems {
				if m.ChatID == "" {
					continue
				}
				repoKey, ok := resolve(ctx, m.ChatID)
				if !ok || repoKey == "" {
					continue
				}
				stat := byRepo[repoKey]
				if stat == nil {
					stat = &RescopeRepoStat{}
					byRepo[repoKey] = stat
				}
				stat.Count++
				if len(stat.Examples) < rescopeExamplesPerRepo {
					stat.Examples = append(stat.Examples, preview(m.Content))
				}
				if apply {
					if err := s.idx.updateBucket(ctx, m.ID, prefixed(bucketRepo, repoKey)); err != nil {
						return nil, fmt.Errorf("memory: rescope update %q: %w", m.ID, err)
					}
					s.logOp(ctx, m.ID, OpUpdate, ActorRescope, "rescope: "+bucket+" -> "+prefixed(bucketRepo, repoKey))
				}
			}
			offset += len(mems)
			if offset >= total || len(mems) == 0 {
				break
			}
		}
	}
	return byRepo, nil
}
