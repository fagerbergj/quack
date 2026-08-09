# GitGraph examples

## Git flow with a hotfix

What this shows: a feature branch merging back to main alongside a separate hotfix branch.

```mermaid
gitGraph
   commit
   commit
   branch develop
   checkout develop
   commit
   commit
   checkout main
   branch hotfix
   checkout hotfix
   commit id: "fix-crash"
   checkout main
   merge hotfix tag: "v1.0.1"
   checkout develop
   commit
   checkout main
   merge develop tag: "v1.1.0"
```

## Feature branch with a cherry-picked hotfix

What this shows: `cherry-pick` to bring one specific commit from another branch onto the current branch.

```mermaid
gitGraph
    commit id: "ZERO"
    branch develop
    branch release
    commit id: "A"
    checkout main
    commit id: "ONE"
    checkout develop
    commit id: "B"
    checkout main
    merge develop id: "MERGE"
    commit id: "TWO"
    checkout release
    cherry-pick id: "MERGE" parent: "B"
    commit id: "THREE"
```

## Trunk-based development with PR merges

What this shows: short-lived branches merged straight back into `main`, a common trunk-based pattern.

```mermaid
gitGraph
   commit
   branch pr-123
   checkout pr-123
   commit id: "add endpoint"
   commit id: "add tests"
   checkout main
   merge pr-123
   branch pr-124
   checkout pr-124
   commit id: "fix typo"
   checkout main
   merge pr-124
```

## Tagged release train with parallel feature branches

What this shows: `HIGHLIGHT` commit type and tags marking releases across multiple in-flight branches.

```mermaid
gitGraph
   commit
   branch featureA
   branch featureB
   checkout featureA
   commit id: "A1"
   checkout featureB
   commit id: "B1"
   checkout main
   commit type: HIGHLIGHT tag: "v1.0.0"
   checkout featureA
   commit id: "A2"
   checkout main
   merge featureA tag: "v1.1.0"
   checkout featureB
   commit id: "B2"
   checkout main
   merge featureB tag: "v1.2.0"
```
