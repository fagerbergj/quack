# Audit: reMarkable document-ingest workflow

- **Sources:** v0 `extensions/remarkable/plugin/workflows/document-ingest.yaml` (two nodes: `media-reader` then `synthesizer`) and `plugin.json` on `main`; the extension's Go code was not audited. The agents it calls are audited in [media.md](media.md); the synthesizer's rubric is covered by [research.md](research.md) (dropped).
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)). Nothing was run and no model was called.
- **v1 side:** [reMarkable requirements](../extensions/remarkable.md) (RMK-1..RMK-6), [reMarkable RFC](../rfc/extensions/remarkable.md), [workflows](../extensions/workflows.md) (WFL-1..WFL-4), D35, RM1, RM2, A15
- **Outcome:** nothing ports from the YAML. v1 needs five steps and the v0 workflow has two. The transcribe prompt comes from [media.md](media.md); the summarize node is dropped; classify and extract-actions have no v0 text and must be written and measured new.

## Effectiveness

None. The repo has no ingest dataset, no recorded run, no golden item and no commit message that reports an ingest result. The extension's history before October is in the archived quack-extensions repo (moved in by #1658), and only a version bump (`remarkable v0.3.0`, 0d7ebbcf) is visible from here. Manual selection rather than auto-ingest is the one evidenced decision: `docs/extensions/remarkable.md` says every autosave bumps `lastModified`, so automatic ingest would process half-written notes. v1 keeps it (RM2, RMK-1).

## Alignment

The v0 definition against what v1 requires:

| v1 requirement | v0 workflow | Gap |
| --- | --- | --- |
| RMK-1 browse and pick | Done by the extension's Go code, not the workflow | None; stays in the service half. |
| RMK-2 searchable transcript research can cite | `transcribe` node output feeds `summarize`; the terminal node is the summary | The transcript is intermediate data. v1 seals it as the `remarkable.transcript` artifact (A15), and research's `quotes_exist` check reads it, so transcript fidelity now matters. |
| RMK-3 classify and file by topic and type | Absent. The extension doc says "the classify/index pipeline is the run", but the definition has no classify step. | New step. The topic and type taxonomy is not defined in any v1 spec; see Follow-ups. |
| RMK-4 todos and questions as gated actions | Only the sentence "key facts, decisions, and action items" in the summarize task, as prose | New step with a schema (one item per todo or question, with its source page) so each can become a gated action such as starting a research session. |
| RMK-5 upload | Not part of the workflow | Separate tool (`remarkable_upload`). |
| RMK-6 push when the batch ends | Not part of the workflow | Service half. |

Node by node:

- **`transcribe`**: task "Transcribe this document's full content, preserving structure (headings, lists, sketches described in words)" plus `{{ask}}`. Keep the intent; the instruction becomes the page-by-page prompt in [media.md](media.md). `{{ask}}` passes the operator's free text into a fixed step, which gives the model a second, unrelated ask; the RFC's per-document submission carries a document id and version, not an ask, so drop it.
- **`summarize`**: "structured Markdown summary: key facts, decisions, and action items". Drop the summary. RMK-1..RMK-6 never ask for one, classification reads the transcript, and the action items become the extract step. A summary adds a model call per document with no consumer and no measure of use (none).
- **`agents`, `shape`, `trigger` fields**: routing text for v0's planner. v1 has no planner for this job (D32, D35), so they have no reader.
- **`plugin.json`**: v0 packaging for the plugin registry. A v1 workflow is code in the extension (WFL-1).

Rubrics: the ingest path has no gate in v0 (the readers run `judge: false`), and the v1 specs give the transcript no checker. The extract step's outputs are gated actions, so the operator approves each (D3, RMK-4); no rubric is needed for them.

## Conciseness

The file is two short task strings; nothing to cut. What is missing is the three steps the spec requires.

## Porting decision

1. Implement `remarkable.ingest` as render (code), transcribe (model, per page), classify (decision point), extract actions (model, schema output), file (code), per the RFC. Port only the transcribe intent from v0.
2. Drop the summarize node, the synthesizer dependency, `{{ask}}`, and the YAML and plugin.json shapes.
3. Write two new prompts with no v0 source: classify and extract. Measure them before they ship (EVL-5 asks for 20 real cases); the first 20 ingested documents are the natural set, labelled by hand.
4. Follow-ups: fix a topic and type vocabulary for classify (open: which labels, and who edits them); decide whether the transcript needs a checker once research citations show whether bad transcripts occur.
