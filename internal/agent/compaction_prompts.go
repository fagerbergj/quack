package agent

// The compaction prompts follow GOOSE, not opencode.
//
// opencode blanks an old tool result in place and keeps the call ("[Old tool
// result content cleared]"), which works when a huge-context frontier model
// rarely triggers prune. On quack's 65k local-model window it fired
// constantly and the agent kept re-reading the same files. The stub is gone
// (see compaction.go).
//
// goose and OpenHands instead DROP old turns and replace them with one
// knowledge-dense summary, framed explicitly as a handoff to the agent
// itself: "ALL TECHNICAL CONTENT" preserved, files/code state kept in full,
// "OK to make it MUCH LONGER than a normal summary". These prompts follow
// that framing - narrative sections stay terse, but file/code-state can run
// long, since a summary that drops what a file contained just makes the
// agent read it again.
const compactionSystemPrompt = `You are compacting the context of a coding/research session.

The conversation history you are given is being REMOVED from the agent's context and replaced by what you write. The only reader is THE AGENT ITSELF, continuing this same session - write a handoff to yourself, not a report for a human.

Generate a version of the history with only the most verbose parts removed. Include the user's requests, the assistant's responses, ALL TECHNICAL CONTENT, and as much of the original context as possible. Anything a tool call revealed - file contents, symbols, command output, errors - is lost the moment you omit it, and the agent will simply redo the call.

If the prompt includes a <previous-summary> block, treat it as the current summary: preserve still-true details, remove stale ones, and merge in the new facts.

This summary will only be read by you, so it is OK to make it MUCH LONGER than a normal summary. Do not exclude any information that might be important to continuing the session. Preserve exact file paths, symbols, commands, and error strings.

Explicitly identify and state the primary language used by the user at the top of your summary (e.g., "Conversation Language: English"). If the agent called any tools, accurately list their exact names to maintain tool grounding - do not paraphrase or invent a tool name.

Do not answer the conversation itself. Do not mention that you are summarizing or compacting. Respond in the same language as the conversation.`

const summaryTemplate = `Output exactly the Markdown structure shown inside <template> and keep the section order unchanged. Do not include the <template> tags in your response.
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
- Do not mention the summary process or that context was compacted.`

// compactionNotice is both the model's framing (so it knows the history it
// can no longer see is folded into the summary rather than lost - goose tells
// the model plainly: "Your context was compacted. The previous message
// contains a summary of the conversation so far.", crates/goose/src/
// context_mgmt/mod.rs) and the SENTINEL: it prefixes the durable summary
// content's first text part, and isSentinel/applyView identify a compaction
// event by this exact prefix. Do not change it without updating both readers.
const compactionNotice = "\n\nYour context was compacted: the older turns of this session are no longer shown, and the summary below is what they contained. Treat it as your own memory of work already done - do not re-read files or re-run commands it already covers.\n\n"
