# reMarkable extension

Status: draft, 2026-10-30. Priority 4 of the launch workloads (D2). Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Turn handwritten notes the operator chooses into searchable text, file them, and propose the actions they contain; send reports back to the tablet (RM1, RM3).

**Shape.** Ingest is a workflow: fetch, transcribe, classify, file, extract actions, with a model only for transcription, classification and extraction (D35).

**Success.** Every chosen document becomes searchable and citable by research (R7), and every extracted action reaches the operator as a proposal.

**Out of scope.** Automatic ingest (RM2); any source other than rmfakecloud (RM3).

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| RMK-1 | MUST | Browse the tablet's documents through rmfakecloud and let the operator pick which to ingest. Nothing is ingested automatically. | RM2, RM3 |
| RMK-2 | MUST | Produce a searchable transcript of each ingested document that research can cite. | RM1, R7 |
| RMK-3 | MUST | Classify and file each document by topic and type. | RM1 |
| RMK-4 | MUST | Extract todos and questions as proposed gated actions, such as starting a research session, that the operator approves. | RM1, D3 |
| RMK-5 | MUST | Upload documents to the tablet (e.g. research reports) as a gated action. | RM3, R6 |
| RMK-6 | MUST | Send a push when an ingest batch finishes. No latency SLO; time per document is a diagnostic. | RM4, D22 |

## Uses from core

Workflows extension, gated actions, the service half for browsing documents and holding the transcript index, credentials injected at run time.

