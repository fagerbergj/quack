package github

import (
	"context"
	"fmt"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

var (
	_ sdk.AssignmentMetaExtension    = (*Extension)(nil)
	_ sdk.AssignmentFreshnessChecker = (*Extension)(nil)
)

// assignmentHookTimeout bounds each hook's GitHub API call; both run inline inside a tool call.
const assignmentHookTimeout = 10 * time.Second

// sessionIDer recovers the dispatching chat id via structural typing -
// quack's adk agent.Context satisfies this without the sdk importing it.
type sessionIDer interface {
	SessionID() string
}

// OnAssignment stamps repo/base_ref/base_sha for this extension's own dispatches (nil otherwise).
// base_ref is the PR head branch when anchored to one, else the branch quack cloned from.
func (e *Extension) OnAssignment(ctx context.Context, a sdk.Assignment) map[string]any {
	sc, ok := ctx.(sessionIDer)
	if !ok {
		return nil
	}
	v, ok := e.pending.Load(sc.SessionID())
	if !ok {
		return nil
	}
	pr := v.(*pendingRun)
	setup := pr.dispatched.Run.Setup
	if setup == nil || setup.Repo == "" {
		return nil
	}
	branch := setup.BaseRef
	if setup.ExistingHeadRef != "" {
		branch = setup.ExistingHeadRef
	}
	if branch == "" {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, assignmentHookTimeout)
	defer cancel()
	sha, err := e.app.branchHeadSHA(tctx, pr.owner, pr.repo, branch)
	if err != nil {
		e.host.Log.Warn("github: OnAssignment tip lookup failed; stamping no meta",
			"repo", pr.owner+"/"+pr.repo, "branch", branch, "err", err)
		return nil
	}
	return map[string]any{"repo": setup.Repo, "base_ref": branch, "base_sha": sha}
}

// BeforeAssignment compares meta.github.base_sha against the branch tip; missing meta is "not mine"
// (fresh), and a lookup failure fails closed rather than risk resuming stale work.
func (e *Extension) BeforeAssignment(ctx context.Context, a sdk.Assignment) (fresh bool, reason string) {
	meta, ok := a.Meta["github"]
	if !ok {
		return true, ""
	}
	baseSHA, _ := meta["base_sha"].(string)
	repoURL, _ := meta["repo"].(string)
	baseRef, _ := meta["base_ref"].(string)
	if baseSHA == "" || repoURL == "" || baseRef == "" {
		return true, ""
	}
	owner, repo, ok := ownerRepoFromURL(repoURL)
	if !ok {
		return true, ""
	}
	tctx, cancel := context.WithTimeout(ctx, assignmentHookTimeout)
	defer cancel()
	tip, err := e.app.branchHeadSHA(tctx, owner, repo, baseRef)
	if err != nil {
		return false, fmt.Sprintf("could not check tip: %v", err)
	}
	if tip == baseSHA {
		return true, ""
	}
	return false, fmt.Sprintf("base moved: %s -> %s", shortSHA(baseSHA), shortSHA(tip))
}
