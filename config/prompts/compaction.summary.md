Output exactly the Markdown structure shown inside <template> and keep the section order unchanged. Do not include the <template> tags in your response.
<template>
## Goal
- Conversation Language: [the primary language used by the user, e.g. "English"]
- [single-sentence task summary]

## Constraints & Preferences
- [user constraints, preferences, specs, or "(none)"]

## Progress
### Done
- [completed work, with its result, or "(none)"]

### In Progress
- [current work or "(none)"]

### Blocked
- [blockers or "(none)"]

## Key Decisions
- [decision and why, or "(none)"]

## Files & Code State
- [every file or directory READ, LISTED, SEARCHED or EDITED, by exact path: what it actually contains - key symbols, signatures, types, structure, the relevant code itself, and for edits the change and why. Be as detailed as needed; this section replaces having read the file. Or "(none)"]

## Commands & Tools Run
- [command or tool call: what it returned - test/build results, failing cases, exit status, key output, or "(none)"]

## Errors & Fixes
- [error string encountered: cause and how it was fixed, or "(none)"]

## Repository State
- [repo, branch, commit, uncommitted edits, PR status, or "(none)"]

## Next Steps
- [ordered pending tasks or "(none)"]

## Critical Context
- [other important technical facts, open questions, or "(none)"]
</template>

Rules:
- Keep every section, even when empty.
- The narrative sections (Goal, Progress, Key Decisions, Next Steps) stay terse bullets. "Files & Code State", "Commands & Tools Run" and "Errors & Fixes" may be as long as they need to be - they carry the knowledge that stops the agent redoing work.
- Preserve exact file paths, commands, error strings, and identifiers.
- Do not mention the summary process or that context was compacted.
