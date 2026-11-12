import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { Artifacts, Questions, type SealedArtifact } from '@quack/sdk';
import { openSeed } from './seed-contracts.ts';

const WEB = fileURLToPath(new URL('../../packages/web/src/extensions/', import.meta.url));
const text = (path: string) => readFileSync(`${WEB}${path}`, 'utf8');
export const LEAGUE = '1356407594683482112';

// Every seeded check is a pitfall (negative weight): it fails when found, so met 1 is a failure.
const check = (criterion: string, id: string, label: string, met: number, notes: string[] = []) => ({ criterion, id, label, weight: -3, met, notes });
const passed = (checks = [check('cites', 'lines', 'Cites a line that is not in the diff', 0)]) => ({ passed: true, gatesFailed: [], score: 1, checks });
const failed = { passed: false, gatesFailed: [], score: 0.6, checks: [check('cites', 'lines', 'Cites a line that is not in the diff', 1, ['publisher.ts:58 is outside the diff.'])] };

const REPORT = `## Short answer

Langfuse v4 moved public ingestion to OpenTelemetry: the \`/api/public/ingestion\` batch endpoint stays for SDK v2 traffic, and new SDKs send OTLP spans to \`/api/public/otel\` [S1].

## What changed

- Traces and observations are written as OTLP spans; the legacy batch shape is still accepted but no longer documented as primary [S1].
- Scores keep their own endpoint, \`POST /api/public/scores\` [S2].
- Media uploads use presigned URLs from \`/api/public/media\` [S3].

## Sources

- S1: Langfuse docs, "OpenTelemetry" (2026-09)
- S2: Langfuse API reference, Scores (2026-10)
- S3: Langfuse changelog, Media support (2026-08)
`;

type Seed = { key: string; title?: string; ask: string; kind: string; versions: Array<Pick<SealedArtifact, 'content'> & { verdict: object }>; question?: string };

const SEEDS: Seed[] = [
  { key: 'github:fagerbergj/quack#2243', title: 'fix(judge): Every failing item is named in the verdict, with its rubric line', ask: 'Review the pull request', kind: 'github.review', versions: [{ content: text('fixtures/review-changes.md'), verdict: failed }, { content: text('fixtures/review-approve.md'), verdict: passed() }] },
  { key: 'research:qa-langfuse', title: 'What changed in the Langfuse v4 public ingestion API? Cite sources.', ask: 'What changed in the Langfuse v4 public ingestion API? Cite sources.', kind: 'research.report', versions: [{ content: REPORT, verdict: passed([check('citations', 'support', 'Cites a source that does not say it', 0)]) }] },
  { key: 'research:qa-ask', title: 'Which tracing backend should quack v1 use?', ask: 'Which tracing backend should quack v1 use?', kind: 'research.report', versions: [{ content: REPORT, verdict: passed() }], question: 'Should self-hosting cost count against a hosted option?' },
  { key: `sleeper:${LEAGUE}:8:lineup`, ask: 'Set the week 8 lineup', kind: 'sleeper.cards', versions: [{ content: (JSON.parse(text('sleeper/fixtures/8-lineup.json')) as { content: string }).content, verdict: passed() }] },
];

// One reply on a canned model, then the versions and any question written straight into the session's docs.
async function seedOne(dataDir: string, s: Seed) {
  const faux = fauxProvider({ models: [{ id: 'seed' }] });
  faux.setResponses([() => fauxAssistantMessage('Done: it is beside this chat.')]);
  const session = await openSeed(dataDir, s.key, s.title ?? '', faux, 'quack');
  await session.submit(s.ask, 'seed-1', true);
  const at = Date.now();
  await session.root.commit(async (tx) => {
    const doc = await tx.doc(Artifacts, session.root.id);
    doc.kinds[s.kind] = { versions: s.versions.map((v, i) => ({ kind: s.kind, version: i + 1, hash: `qa${i}`, content: v.content, sealedAt: at + i, verdict: v.verdict as SealedArtifact['verdict'] })) };
    if (s.question) (await tx.doc(Questions, session.root.id)).items['qa:scope'] = { question: s.question, options: ['Yes', 'No'], evidence: null, title: s.question, freeText: true, askedAt: at, answer: null, closed: false };
  }, ctx);
  await session.close();
}

/** Sessions with sealed versions of each kind the pane draws, one with a question waiting; written into a data dir before the distributor starts. */
export async function seedArtifacts(dataDir: string) {
  for (const s of SEEDS) await seedOne(dataDir, s);
}
