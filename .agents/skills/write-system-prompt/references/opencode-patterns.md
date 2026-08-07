# OpenCode System Prompt Patterns

## Architecture Overview

OpenCode assembles the system prompt from four collaborating sources, orchestrated by `packages/opencode/src/session/prompt.ts` [source](https://gist.github.com/rmk40/cde7a98c1c90614a27478216cc01551f):

1. **Environment block** (`system.ts`) - Injects model name, working directory, platform, and current date
2. **Provider-specific prompt** - Selected by matching model ID:
   - Claude → `anthropic.txt` (PROMPT_ANTHROPIC)
   - GPT/o1/o3 → `beast.txt` (PROMPT_BEAST)
   - GPT-5 → `codex_header.txt` (PROMPT_CODEX)
   - Gemini → `gemini.txt` (PROMPT_GEMINI)
   - Trinity → `trinity.txt` (PROMPT_TRINITY)
   - Others → fallback to `qwen.txt` or PROMPT_ANTHROPIC_WITHOUT_TODO
3. **Instruction files** - Walks from working directory upward looking for `AGENTS.md`, `CLAUDE.md`, or `CONTEXT.md`; stops at the first found. Also resolves URL-based instructions from config, prefixed with `Instructions from: <path>`
4. **Agent overrides** - Built-in agents (`build`, `plan`, `explore`, `general`, `compaction`, `title`, `summary`) can define their own prompts that replace the provider prompt entirely

## Mode Fragments

Small `.txt` files are injected at runtime based on session state, not baked into the static system prompt [source](https://gist.github.com/rmk40/cde7a98c1c90614a27478216cc01551f):

- `plan.txt` - Appended to last user message when plan mode is active
- `build-switch.txt` - Injected when switching from plan → build mode
- `max-steps.txt` - Sent as a fake assistant message when step limit is exceeded

## Plugin Hooks

A plugin hook (`experimental.chat.system.transform`) allows plugins to mutate the system array.
Safety fallback restores original if the result is empty [source](https://gist.github.com/rmk40/cde7a98c1c90614a27478216cc01551f).

## Cache Optimization

For Anthropic's prompt caching: if the first element of the system array survived plugin transforms unchanged, the rest is joined into a single string to maintain a cacheable two-part structure [source](https://gist.github.com/rmk40/cde7a98c1c90614a27478216cc01551f).

## Key Design Principles Observed

- **Provider specificity matters**: Each LLM provider has different response characteristics; one-size-fits-all prompts are suboptimal
- **Layered instructions win**: AGENTS.md at project level overrides global defaults - this hierarchy should be documented in any prompt a user writes
- **Tool protocol belongs in the body, not headers**: Tool-specific guidance should live in `AGENTS.md` or agent configs, not the core system message (keeps the base prompt lean)
