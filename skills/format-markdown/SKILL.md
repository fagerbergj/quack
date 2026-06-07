---
name: format-markdown
description: Use before returning any Markdown document to the user. Reformats it for clean visual rendering — consistent whitespace, heading hierarchy, scannability, and formatting consistency. Preserves all content and links.
---
# Format Markdown Skill

Reformat the provided Markdown document for clean visual rendering.
Output only the reformatted document — no preamble, no commentary.

## Rules

- Blank line before and after every heading, code block, blockquote, list, and horizontal rule.
- At most three heading levels (H1 for the title only, H2 for sections, H3 for subsections). Never skip levels.
- Break any paragraph longer than 4–5 sentences into shorter ones or a list.
- Prefer a tight bulleted list over a sentence enumerating 3+ items. For labeled items, bold the label: **Label** — description.
- Always declare the language on fenced code blocks (` ```python `, ` ```bash `, etc.).
- Bold only for labels and genuinely critical terms — not for decorative emphasis.
- At most one blockquote per major section, for notes or warnings only.
- At most one or two horizontal rules per document as major section dividers.
- Preserve all inline links, URLs, and Sources sections exactly. Do not alter any URL or link text.
