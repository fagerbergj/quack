---
name: <skill-name-here>
# ≤64 chars; lowercase a-z, 0-9, hyphens only; no leading/trailing '-'; no '--'; must match folder name exactly
description: |
  <What this skill does AND when to use it. Include specific trigger keywords and user-request phrases.
  Target: 2–4 sentences. Max 1,024 characters. Be explicit - agents undertrigger on vague descriptions.>
license: MIT
# compatibility: <omit unless the skill has real environment requirements; ≤500 chars>
metadata:
  author: <handle>
  version: "1.0"
# allowed-tools: Bash Read Write Edit   # experimental; omit if unsure
---

# <Skill Title>

## Overview

<One paragraph: what the skill does and where it stops. Define scope boundaries so the agent knows when this task starts and ends.>

## When to Use

<Specific trigger phrases, user request patterns, or contexts that should activate this skill.>

- Example: "When the user mentions 'dashboard', 'metrics report', or 'display internal data'."
- Example: "Whenever a PDF is uploaded, attached, or discussed."

## When NOT to Use

<Explicit misfire prevention - scenarios where this skill should not be invoked.>

- Example: "Do not use when the user wants to generate a new PDF; this skill only reads and extracts."

## Step-by-Step Procedure

<Numbered steps. Use checkboxes for multi-step workflows. Calibrate prescriptiveness per step: exact commands for fragile operations, explain *why* for open-ended ones.>

- [ ] Step 1: <Action with explicit file paths and expected inputs>
- [ ] Step 2: <Tool or command to run>
- [ ] Step 3: <Validation - what to check, when to fail over>

> For fragile operations: "Run exactly `<command>`. Do not modify the command or add additional flags."

## Gotchas

<Environment-specific facts that defy reasonable assumptions. This is the highest-value section - add entries whenever an agent makes a mistake during iteration.>

- <Example: "The API uses camelCase in URLs but snake_case in request bodies.">

## Validation Loop

<How the agent validates its own work before delivering. Do work → run validator → fix → repeat.>

Run `scripts/validate.sh` after completing each major step. If it fails, read the error and retry before delivering.

## Resources

<One line per file with an explicit trigger condition. Never use "see references for details" without a trigger.>

- Read `references/<topic>.md` if <specific trigger, e.g. "the API returns a 4xx error">.
- Run `scripts/<script>.sh` to <purpose>.

---

## Pre-Submission Checklist

Run every item before delivering. Iterate until all pass.

- [ ] `SKILL.md` exists at the skill root.
- [ ] Frontmatter YAML is well-formed and delimited by `---`.
- [ ] `name`: ≤64 chars, lowercase alphanumeric + hyphens, no leading/trailing `-`, no `--`, matches parent directory name exactly.
- [ ] `description`: ≤1,024 chars, non-empty, covers **what** and **when**, includes specific trigger keywords.
- [ ] `license` (if present): short name or bundled file reference.
- [ ] `compatibility` (if present): ≤500 chars; omitted if no real requirements.
- [ ] `metadata` (if present): flat string key-value map with unique keys.
- [ ] `allowed-tools` (if present): space-separated tool names; noted as experimental.
- [ ] Body is ≤500 lines / ~5,000 tokens.
- [ ] File references use relative paths from the skill root, reachable in one hop (no reference chains).
- [ ] Resource directories are `references/`, `assets/`, or `scripts/` (portable across runtimes).
- [ ] Every file reference has a specific trigger condition (not a generic pointer).
- [ ] Skill covers one coherent unit of work.
- [ ] Provides one default per decision point; alternatives mentioned briefly.
- [ ] Favors step-by-step procedures over one-off declarations.
- [ ] Includes gotchas for non-obvious environment-specific facts.
- [ ] Uses checklists or validation loops for multi-step or structured output.
- [ ] Detail moved out of SKILL.md body into `references/`, `assets/`, or `scripts/`.
