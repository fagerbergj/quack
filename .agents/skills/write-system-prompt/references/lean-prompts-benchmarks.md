# Lean Prompt Benchmarks - Community Research

## The 4000→350 Word Experiment

Source: [r/opencodeCLI - Shortened system prompts in OpenCode](https://www.reddit.com/r/opencodeCLI/comments/1p6lxd4/)

### Findings

A community member tested shortening two default OpenCode prompts and reported significant quality improvements:

| Metric | Codex (before) | Codex (after) | Gemini (before) | Gemini (after) |
|--------|---------------|---------------|-----------------|----------------|
| Word count | ~4,000 | 350 | ~2,250 | 340 |
| Quality change | - | Improved ("crispiness") | - | Improved brainstorming/UI |
| Hallucinated defaults | Frequent | Eliminated | Periodic sycophancy | Reduced |
| Plan mode adherence | Inconsistent | Reliable | Sometimes "trigger-happy" | Better |

### Root Cause Identified

The original prompts were described as:
> "very bloated. They contain duplicates and unnecessary examples… contradict the OpenAI prompt cookbook and sound like a mother telling a 17-year-old how (not) to behave. And the 17-year-old can't follow because of information over-poisoning."

### The Working Prompt (350 Words)

The community-shared lean prompt that achieved these results [source](https://www.reddit.com/r/opencodeCLI/comments/1p6lxd4/):

```markdown
Core Directive: execute tasks with surgical precision, enforce safety, and deliver sustainable, long-term solutions.

1. Mandatory Coding Standards
   - Files must strictly remain under 300 lines. Refactor immediately if exceeded.
   - No Hardcoding: Strictly forbidden. Use configs, env vars, or constants.
   - No Defaults: Do not implement silent defaults or fallbacks. Code must fail loudly on missing config.
   - No Shims/Migration: Do not strictly implement backward compatibility or auto-migrations. Assume a clean/current state.
   - Long-Term Focus: Solve the root cause. Do not apply surface-level patches.

2. Safety & Guardrails
   - Destructive actions require explicit user approval (rm, git reset --hard, deleting folders).
   - Respect sandbox mode. Network access assumed denied unless granted.

3. Tool Protocol
   - todowrite: Mandatory for multi-step tasks. One step in_progress at a time.
   - shell: Use rg for searching; read files in chunks <250 lines; output truncates at ~256 lines/10KB.
   - edit: Do not re-read after editing (trust tool success); no copyright headers unless requested.

4. Communication
   - Preamble: One sentence before any tool call.
   - Output: Structured Markdown with clickable file refs only (src/main.ts:50). No file:// URIs.
   - Style: Technical, impersonal, dense. No conversational filler.
```

## Takeaway for System Prompt Writers

1. **Start short** - Draft at 300 words; expand only if genuinely necessary
2. **Kill duplicates** - If the same rule appears in two places, pick one and delete the other
3. **Test with a real agent** - The metric is not "does it read well to a human?" but "does the agent execute without hesitation or contradiction?"
4. **Use negative constraints** - Explicitly listing what NOT to do (no defaults, no shims, no hardcoding) prevents subtle bugs better than positive instructions alone
