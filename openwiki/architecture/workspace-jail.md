---
type: Architecture Document
title: Workspace Isolation (Jail)
description: The workspace jail - a per-user filesystem containment boundary that every file and git tool resolves through. Covers Jail struct, path resolution rules (userID/chatID scoping, ErrEscape/ErrInvalidUserID errors), the shared vs plan-judge reserved scopes, HomeDir fix, and NodeDir per-node isolation.
tags: [workspace, jail, security, isolation, filesystem]
resource: /internal/workspace/jail.go
---

# Workspace Isolation (Jail)

The workspace jail is the single filesystem containment boundary every file-read and git tool resolves through. It provides per-user scoping with strict path validation - no `..` escapes, no absolute paths, no symlink traversal outside the scope. The package is intentionally dependency-free (stdlib only: `os`, `path/filepath`).

## Design Goals

- **User isolation**: `Resolve("alice", …)` and `Resolve("bob", …)` never see each other's files
- **Path safety**: Every path - from user IDs, chat IDs, relative paths, or symlinks - is validated before resolution
- **One error for escapes**: All escape attempts (`..`, absolute paths, out-of-bounds symlinks) return a single uniform `ErrEscape` so models learn one thing, not a taxonomy of jail failures
- **Distinct error boundaries**: An invalid userID (`ErrInvalidUserID`) or chatID (`ErrInvalidChatID`) is a *caller* bug (identity source), not a model-chosen path. These errors help operators fix misconfiguration rather than confusing agents.

## Directory Layout

```
<root>/                    # Jail.root (absolute, symlink-resolved)
├── <user_id>/            # per-user jail root
│   ├── .quack-home/      # dedicated $HOME for spawned child processes
│   ├── <chat_id>/        # per-chat scope
│   │   ├── <node_id>/    # NodeDir - one node's working directory
│   │   └── quack-shared-repo/  # SharedRepoScope - shared clone+branch across nodes
│   └── quack-plan-judge/ # PlanJudgeScope - plan judge's read-only clone
└── <user_id2>/           # Another user's jail (completely isolated)
```

## Key Functions

### `Jail.NewJail(root string)` ([`internal/workspace/jail.go`](/internal/workspace/jail.go))

Creates or validates a Jail rooted at `root`. The root is canonicalized: made absolute and symlinks resolved so later containment checks compare real paths, not aliases. Created with `0o755`.

### `Jail.Resolve(userID, chatID, relPath string) (string, error)`

The **one** path-resolution function every filesystem and git tool uses:

1. Validates `userID` - must be exactly ONE safe path component (no separators, no dots). Rejects with `ErrInvalidUserID`.
2. Validates `chatID` - same rule if non-empty; empty is valid and resolves to per-user root. Rejects non-empty invalid IDs with `ErrInvalidChatID`.
3. Joins `relPath` under the scope root (`<root>/<userID>/<chatID>` or `<root>/<userID>`).
4. Cleans, resolves symlinks on the deepest existing ancestor, and verifies prefix containment in the scope root.
5. Rejects absolute paths, `..` escapes, and out-of-scope symlinks as `ErrEscape`.

### `Jail.UserRoot(userID string)`

Returns `<root>/<userID>` (unresolved for symlinks). Does not create the directory - callers that need it to exist create it themselves.

### `Jail.HomeDir(userID string) (string, error)`

Returns a dedicated `$HOME` for spawned child processes (`run_command`, checks, git tools). Path: `<root>/<userID>/.quack-home/`. Created with `0o700` so toolchain caches/config are not world-readable. This was introduced to fix a live bug where HOME was pinned to a coding task's working directory (the target repo itself), causing `npm ci` to write its cache directly into the repo tree and `git_commit`'s `add_all` sweeping up thousands of cache files alongside real changes.

### `NodeDir(nodeID string) string`

Returns the per-node working directory name - one safe component under `<root>/<userID>/<chatID>/`. If `nodeID` is not a safe path component (e.g., contains separators from planner-authored IDs), returns `""`, falling back to chat root. This was introduced to fix a live bug where concurrent nodes in one chat all cloned into the same directory, so an explorer studying OpenHands saw goose's source sitting there too.

### `SetupCloneDir(nodeID string) string`

Returns `NodeDir(nodeID)` - the workspace-relative directory a plan's declared Setup PRE-step clones a repo into. The repo IS the node's own root, so relative paths resolve without a "repo/" prefix and without forcing absolute paths.

## Reserved Scopes

### `SharedRepoScope = "quack-shared-repo"` ([`internal/workspace/jail.go`](/internal/workspace/jail.go))

A reserved node identifier for DAG nodes that share ONE declared Setup clone+branch across a `depends_on` chain (typically code-implementer → code-reviewer). Instead of each node getting its own dir, they resolve to this shared path. It is quack-authored, never planner-chosen, so it cannot collide with a real node ID.

### `PlanJudgeScope = "quack-plan-judge"` ([`internal/workspace/jail.go`](/internal/workspace/jail.go))

A reserved node identifier for the plan judge's own grounding clone (used by `vetting.NewPlanJudge`). Deliberately DISTINCT from `SharedRepoScope` so the judge's read-only clone can never race with or be clobbered by the DAG's clear-then-clone of the same repo into `SharedRepoScope`.

## Error Taxonomy

| Error | When returned | Meaning |
|-------|--------------|---------|
| `ErrEscape` | Any path would resolve outside scope | Model or caller attempted path traversal |
| `ErrInvalidUserID` | userID fails single-component validation | Identity source misconfiguration (operator problem) |
| `ErrInvalidChatID` | Non-empty chatID fails single-component validation | System-generated UUID that doesn't conform to containment rules |

## Related Concepts

See also: [System Architecture](/architecture/overview.md) · [Adversarial Trust Gate](/architecture/vetting.md) · [DAG Execution](/workflows/dag-execution.md)
