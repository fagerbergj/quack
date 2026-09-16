---
name: format-markdown
description: >
  Reformats a Markdown document for clean visual rendering - consistent whitespace,
  heading hierarchy, scannability, and formatting consistency. Preserves all content
  and links exactly. Use before returning any Markdown document to the user, or when
  asked to clean up, format, or tidy Markdown output.
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0"
---

# Format Markdown Skill

## Overview

Reformat the provided Markdown document for clean visual rendering. Output only the reformatted document - no preamble, no commentary. This skill shapes whitespace and structure; it never alters meaning, links, or factual content.

## When to Use

- Before returning any Markdown document to the user.
- When the user asks to "clean up", "format", "tidy", or "fix the formatting of" a document.
- When a Markdown answer has inconsistent heading levels, missing blank lines, or disorganized lists.

## When NOT to Use

- Do not apply this skill to code files (`.go`, `.ts`, etc.) - use a language formatter instead.
- Do not apply if the user explicitly wants the original whitespace preserved (e.g., poetry, source diffs).
- Do not alter content when formatting: if fixing whitespace would require rewriting a sentence, leave it.

## Rules

- Do not hard-wrap prose at a fixed column. Write one line per paragraph (and one line per list item) and let it soft-wrap. Fixed-width wrapping is an inherited print convention that produces noisy diffs and fights machine editing - a one-word change should not reflow a whole paragraph.
- Blank line before and after every heading, code block, blockquote, list, and horizontal rule.
- At most three heading levels (H1 for the title only, H2 for sections, H3 for subsections). Never skip levels.
- Break any paragraph longer than 4–5 sentences into shorter ones or a list.
- Prefer a tight bulleted list over a sentence enumerating 3+ items. For labeled items, bold the label: **Label** - description.
- Always declare the language on fenced code blocks (` ```python `, ` ```bash `, etc.).
- Bold only for labels and genuinely critical terms - not for decorative emphasis.
- At most one blockquote per major section, for notes or warnings only.
- At most one or two horizontal rules per document as major section dividers.
- Preserve all inline links, URLs, and Sources sections exactly. Do not alter any URL or link text.

## Gotchas

- **Blank lines inside `<details>` blocks are required.** The Markdown list inside a `<details><summary>Sources</summary>` block only renders if there is a blank line after `<summary>` and before `</details>`. Do not remove these blank lines when formatting.
- **H1 is for the document title only.** If the document has no title and multiple H2 sections, do not promote an H2 to H1 - add a title only if the document clearly needs one.
- **Don't normalize prose.** This skill reformats structure, not sentences. Do not rephrase, expand, or condense text while formatting.

## Validation

After reformatting, verify:
1. No heading level is skipped (H1 → H3 without an H2).
2. Every fenced code block has a language tag.
3. Every link and URL is unchanged from the original.
4. All `<details>` blocks have their required blank lines.
