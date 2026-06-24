---
name: collect-document-metadata
description: >
  How to gather a document's metadata — title, series, date — before saving it.
  Load this when ingesting/saving a document so the persisted record is
  well-titled and grouped, escalating from the user's prompt → corpus reasoning →
  asking the user only when it still matters.
---

# Collect Document Metadata

Before a document is persisted, fill in its metadata so it's findable later:
**title** (concise, human-readable), **series** (a grouping for related documents,
e.g. a project or recurring meeting — optional), and **date_month** (`YYYY-MM`,
when the document is dated). Tags and the summary come from the classifier — do
not collect those here.

Resolve each field by escalating through three tiers, stopping as soon as you
have a confident value. **Don't ask the user for what you can infer.**

## 1. The user's prompt

Take what the user already told you. "Save these notes from the Q3 planning
meeting" gives a title ("Q3 planning meeting notes") and likely a series ("Q3
planning"). Pull dates, project names, and titles straight from their words before
doing anything else.

## 2. Self-reason + read the corpus

For anything the prompt didn't settle, infer it from the document's own content
and from what's already stored:

- Derive a **title** from the document's subject when the user didn't give one.
- Infer **date_month** from dates in the content.
- For **series**, check whether this belongs with existing documents:
  `semantic_search_document` (and `search_document` for exact names) to find
  similar/related docs, then `load_document` to confirm what series they use.
  If a clear group exists, reuse its series name exactly so the document joins it.

## 3. Ask the user — only if it still matters

Use `get_user_choice` for a field that is **still unresolved AND materially
affects grouping or retrieval** — most often `series` when corpus search surfaces
a couple of plausible groups. Offer the inferred candidates as the options (plus
"none / new series"). Keep it to one question; don't interrogate.

For fields that don't materially matter (a slightly imperfect title, a missing
optional series), prefer a sensible default over a question.

## Hand-off

Pass the collected `title`, `series`, and `date_month` into the doc-ingest plan's
persist step (the `document-writer`'s `create_document` call), alongside the
cleaned content and the classifier's tags + summary.
