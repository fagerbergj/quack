---
name: research-git-repos
description: >
  How to research a git repository's code, structure, or conventions: clone it
  shallowly and read it locally with your filesystem tools instead of fetching
  github.com pages over the web. Load when a task involves understanding a
  codebase, a repo's layout, its conventions, how a feature is implemented, or
  anything answerable from the repository's own files.
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0.1"
---

Researching a repository through web_fetch of github.com pages is the wrong tool: each page fetch burns tokens on HTML chrome for a fraction of one file, directory listings paginate, and rendered blobs truncate. The repository itself is cheaper and complete.

## The workflow

1. Clone the repo shallowly (shallow is the default - history costs nothing to skip when you're reading code). Clone once; every later step is local.
2. Move your working directory into the clone. Every later path is then repo-relative - `README.md`, not `<repo>/README.md` - and the repo's own context comes with it: the nearest AGENTS.md/CLAUDE.md (the project's instructions - read and FOLLOW them) and any project-level skills that repo defines, which you can load. Then orient: read the README and list the tree two levels deep for the layout.
3. Find by shape, then read: match filename patterns (`**/*.test.ts`, `**/Dockerfile`), search the contents for symbols, registrations, or phrases - and open only the hits. Grep-then-read is the token discipline: never page through files hunting.
4. Conventions live in examples: to learn "how do X here", find ONE existing X (the newest, or the one the README names) and read it end to end - its imports, its tests, how it registers itself.
5. Cite what you read as `<repo>@<path>` (e.g. `games@app/games.ts`) - file paths from the clone are your sources; you retrieved them.

## When the web is still right

- Issues, pull requests, discussions, release notes - that's repository METADATA, not repository contents; it only exists on the forge. Fetch those over the web.
- Comparing many repos shallowly (stars, activity, one-line purpose) - search the web first, clone only the ones that survive the cut.

## Cost sanity

A shallow clone of a typical repo is one tool call; reading ten files locally is ten cheap calls with exact content. The same ground via web_fetch is ten HTML pages of chrome, each partially truncated. Clone unless you have a reason not to.
