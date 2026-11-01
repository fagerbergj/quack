# Audit: image-reader and media-reader

- **Sources:** v0 `agents/image-reader` (prompt 387 words, rubric 1,066, card 383) and `agents/media-reader` (prompt 381, rubric 1,015, card 387) on `main`
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)). Nothing was run and no model was called.
- **v1 side:** [reMarkable requirements](../extensions/remarkable.md) (RMK-2), [reMarkable RFC](../rfc/extensions/remarkable.md) (the transcribe step), [workflows](../extensions/workflows.md) (WFL-1, WFL-2), D32 (no router), D35
- **Outcome:** port one transcription prompt of about 150 words for the reMarkable workflow's transcribe step. Drop both agents as separate bundles, both rubrics, both cards and the audio path.

## Effectiveness

There is no measurement of either agent. The repo has no dataset, no golden item (`testdata/judge-golden/` holds web and review items only), no character-error check and no recorded outcome for a transcription. What exists is history:

| Change | What it shows |
| --- | --- |
| c9cf2c19, 665e4c2d, 518201b5 (June 2026) | media-reader came first; image-reader was added for Qwen3-VL-32B after the orchestrator refused image requests instead of planning them. The split between the two is a routing and model choice, not a measured quality gain. |
| #303 | The `clean_output` deliberation clause was copied into both rubrics. The leak it fixed was an orchestrator plan comment, not a reader output. |
| `config/quack.yaml` | Both agents run with `judge: false`, commented "single-pass transcription with no rubric criteria to judge". The two rubric files (2,081 words together) gate nothing in the shipped config. |
| 665e4c2d | Gave the judge the user's image bytes so it could check visual claims, before the agents were switched to `judge: false`. No result is recorded. |

The prompts share one skeleton (rules, four steps, Markdown output, the same three-section template) and differ in wording and scope. image-reader adds handwriting and degraded-text lines and ASCII art for diagrams; media-reader adds audio, a "describe this photo" mode and mermaid for flows.

## Alignment

The one v1 consumer is the reMarkable workflow: render pages to images (code), transcribe with a vision model, classify, extract, file. The transcript is sealed and searched, and research cites it (RMK-2, A15). v1 has no audio requirement and no router (D32), so the card skills that told v0's planner which reader to pick have no reader.

Instruction by instruction:

| Instruction | Decision |
| --- | --- |
| Read every word; state a specific only when clearly visible | Keep. This is what makes a transcript citable, and research's `quotes_exist` check compares quoted spans to the transcript text. |
| Committed best reading, with `[unclear: a/b?]` for real ambiguity | Keep one marker. The v0 prompts use `[unreadable]`, `[unclear: x/y?]`, `[unclear]` and `[illegible]` across the two files; pick one so code can strip or count it. |
| Natural reading order; handwriting uses context to settle a character | Keep reading order. The handwriting sentence is untested; keep it because it targets the stated job. |
| Structure as Markdown (headings, nested lists, tables) | Keep, cut to one line. |
| Diagrams as ASCII art (image-reader) or mermaid (media-reader) | Drop. The v0 workflow task says "sketches described in words", which contradicts both prompts, and words are what the index can search. |
| Output template with `Summary`, named sections and `Uncertain` | Drop. v0's `document-ingest` has a separate summarize node, and in v1 classification and action extraction are their own steps. A transcription step that also summarizes duplicates them. |
| "With no image attached, say so in one sentence and stop"; "no legible text gets one sentence" | Drop. Code renders the pages, so a missing image is a step error, and a blank page needs one rule at most: write "[blank page]". |
| "Open with the answer; 'I can see' is preamble" | Drop. The output is a document, not a chat reply. |
| Audio handling | Drop. No v1 requirement. |

Rubrics. Nothing ports as a model rubric.

| v0 criterion | Decision |
| --- | --- |
| `answers_question` | Drop. The ingest step has one fixed ask. |
| `clean_output` | Drop. Same reasoning as research's audit: the output is a file, not a chat reply. |
| `completeness` (image-reader) | Port as a code check, not a judge: the transcript has one section per rendered page and none is empty. This needs the step to transcribe page by page, which also makes it resumable per page (WFL-2) and fits the 32,768-token context both agents are configured with. A notebook of many pages in one call would not. Untested; confirm on the first real documents. |
| `grounded` | Defer. Checking a transcript against the page image needs a vision judge. v0 tried it in 665e4c2d and then turned the judge off, and the specs give the transcript no checker (the gates are on external effects, D3). Revisit only if wrong transcripts show up in research citations. |
| `internally_consistent` (media-reader) | Drop. A transcript does not argue. |

## Conciseness

Both prompts run about 385 words; the port is about 150 (job and fidelity rule, unclear marker, reading order, Markdown structure, blank-page rule, per-page scope). The cards (about 385 words each) are routing metadata and are dropped whole.

## Porting decision

1. One prompt, `remarkable.transcribe`, built from image-reader (the handwriting-capable one). Model named in the workflow definition per the RFC; v0 used Qwen3-VL-32B for hard reading.
2. Transcribe page by page; the only check is the code check for one non-empty section per page. No model rubric until there is a labelled set of pages (none exists).
3. Dropped: media-reader and the audio path, both rubrics, both cards, the output template, the diagram rules.
