---
name: research-git-repos
description: >
  How to research a git repository's code, structure, or conventions: clone it
  shallowly and read it locally with your filesystem tools instead of fetching
  github.com pages over the web. Load when a task involves understanding a
  codebase, a repo's layout, its conventions, how a feature is implemented, or
  anything answerable from the repository's own files.
---

Researching a repository through web_fetch of github.com pages is the wrong tool: each page fetch burns tokens on HTML chrome for a fraction of one file, directory listings paginate, and rendered blobs truncate. The repository itself is cheaper and complete.

## The workflow

1. `git_clone` the repo (shallow is the default - history costs nothing to skip when you're reading code). Clone once; every later step is local.
2. `cd` into the clone. This moves your working directory into the repo (so every later path is repo-relative - pass `README.md`, not `<repo>/README.md`) AND loads the repo's own context: it returns the nearest AGENTS.md/CLAUDE.md (the project's instructions - read and FOLLOW them) and the project-level skills that repo defines (loadable with `load_skill`). Then orient: `read_file` the README, `list_dir` (depth 2) for the layout.
3. Find by shape, then read: `glob` for filename patterns (`**/*.test.ts`, `**/Dockerfile`), `grep` for symbols, registrations, or phrases - and only `read_file` the hits. Grep-then-read is the token discipline: never page through files hunting.
4. Conventions live in examples: to learn "how do X here", find ONE existing X (the newest, or the one the README names) and read it end to end - its imports, its tests, how it registers itself.
5. Cite what you read as `<repo>@<path>` (e.g. `games@app/games.ts`) - file paths from the clone are your sources; you retrieved them.

## When the web is still right

- Issues, pull requests, discussions, release notes - that's repository METADATA, not repository contents; it only exists on the forge. `web_fetch` those.
- Comparing many repos shallowly (stars, activity, one-line purpose) - `web_search` first, clone only the ones that survive the cut.

## Cost sanity

A shallow clone of a typical repo is one tool call; reading ten files locally is ten cheap calls with exact content. The same ground via web_fetch is ten HTML pages of chrome, each partially truncated. Clone unless you have a reason not to.
