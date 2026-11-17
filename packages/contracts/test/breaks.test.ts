import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { buildEvidence, codeResults } from '@quack/judge';
import type { RecordedSession, SealedArtifact } from '@quack/sdk';
import { contractDeclaration, contractRubric, recordedSpecInputs, type ContractSpec } from '../src/index.ts';
import { unnamedBreaks, type Listed } from '../src/breaks.ts';
import type { Break } from '../src/oasdiff.ts';

// Shared with the web page's warning (packages/web/src/extensions/contracts/studio.test.tsx): the gate fails exactly the breaks it warns on.
const CASES = JSON.parse(readFileSync(new URL('../../../test-fixtures/break-names.json', import.meta.url), 'utf8')) as Array<{ case: string; listed: Listed[]; found: Break[]; unnamed: string[] }>;
for (const c of CASES) test(`a checker break is named only by a whole word in an entry: ${c.case}`, () => assert.deepEqual(unnamedBreaks(c.listed, c.found), c.unnamed));

// The 2026-11-16 shape: one root change, and the exports that carry it break with it.
const V1 = `/** A check. */\nexport interface CheckDecl { decisionModel?: false }\n/** All checks. */\nexport interface Declaration { checks: Record<string, CheckDecl> }\n/** What a kind is judged by. */\nexport interface Judged { declaration: Declaration }\n/** Registers a kind. */\nexport declare function judged(kind: string, how: Judged): void;\n`;
const V2 = V1.replace('decisionModel?: false', "decisionModel?: false | 'pass-only'");
const spec = (version: number, document: string, compatibility?: ContractSpec['compatibility']): ContractSpec => ({
  kind: 'typescript', title: 'Judge', version, intent: { purpose: 'p', nonGoals: [], audience: 'a', consumers: ['extensions'], guarantees: [] }, document, choices: [],
  ...(compatibility && { baseline: 1, changes: [{ choice: 'c', was: 'a', now: 'b', why: 'w' }], compatibility }),
});
const sealed = (version: number, s: ContractSpec, at: number): SealedArtifact => ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(s), sealedAt: at, verdict: { passed: true } as never });
const approved: SealedArtifact = { kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version: 1, artifact: 1, hash: 'h1', note: '', approval: 'a1' }), sealedAt: 5, verdict: null };
const root = { what: "CheckDecl.decisionModel widens from false to false | 'pass-only'", callers: ['packages/judge/src/declaration.ts:203'], migration: "Accept 'pass-only'" };

async function breakingNamed(compatibility: ContractSpec['compatibility']) {
  const next = sealed(2, spec(2, V2, compatibility), 10);
  const session = { docs: {}, artifacts: { 'contract.spec': [sealed(1, spec(1, V1), 1), next], 'contract.approved': [approved] }, readFile: async () => undefined } as unknown as RecordedSession;
  const inputs = await recordedSpecInputs(session, next.content);
  return codeResults(contractRubric, next.content, (await buildEvidence(contractDeclaration, inputs)).evidence).find((r) => r.id === 'breaking_named')!;
}

test('breaking_named fails a spec naming one of several checker breaks, listing the unnamed ones and the safe note that calls them safe', async () => {
  const one = await breakingNamed({ breaking: [root], safe: ['Judged and judged are unchanged.'] });
  assert.equal(one.met, 0);
  assert.deepEqual(one.notes, ["the kind's diff found breaks no compatibility.breaking entry names: Declaration, Judged, judged. Name each as a whole word in the what, callers or migration of the entry whose change causes it (one entry may name several), or add an entry for it. compatibility.safe[0] calls Judged, judged safe, but the kind's diff found them breaking."]);
  const all = await breakingNamed({ breaking: [{ ...root, callers: [...root.callers, 'Declaration, Judged and judged carry CheckDecl'] }], safe: ['Judged keeps its other fields.'] });
  assert.deepEqual([all.met, all.notes], [1, []], 'one entry covers the transitive breaks by naming them, and a safe note on a named export is not a contradiction');
});
