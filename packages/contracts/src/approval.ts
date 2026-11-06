import { createHash } from 'node:crypto';
import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineTool, GenerationTask, hook, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, gated, type Approval, type SealedArtifact } from '@quack/sdk';
import { kinds } from './kinds.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec } from './spec.ts';

export const APPROVED_KIND = 'contract.approved';
export const CHANGES_KIND = 'contract.changes';

const reply = (text: string, isError = false) => ({ output: [{ type: 'text' as const, text }], isError });
const METHOD = /^(get|put|post|delete|patch)$/;

/** "GET /a, POST /b": the routes a REST document defines, which name the contract better than its title. */
export function routesOf(document: unknown): string {
  const paths = (document as { paths?: Record<string, Record<string, unknown>> })?.paths ?? {};
  const ops = Object.entries(paths).flatMap(([p, item]) => Object.keys(item).filter((m) => METHOD.test(m)).map((m) => `${m.toUpperCase()} ${p}`));
  return ops.length > 2 ? `${ops.slice(0, 2).join(', ')} +${ops.length - 2}` : ops.join(', ');
}

/** What a proposal asks the person to approve: the spec version and the exact sealed artifact, by number and hash. */
type Proposed = { version: number; artifact?: number; hash?: string };

// Writes the artifact for every decided proposal that has none, keyed by approval id, so a crash between the verdict and the record loses nothing.
async function reconcile(api: ToolExecutionApi, ctx: Context) {
  await api.commit(async (tx) => {
    const { items } = await tx.doc(Approvals, api.conversationId);
    const doc = await tx.doc(Artifacts, api.conversationId);
    for (const [id, a] of Object.entries(items)) {
      if (a.tool !== 'propose_contract' || !a.verdict) continue;
      const { version, artifact, hash } = a.args as Proposed;
      const ok = a.verdict.decision === 'approve';
      const kind = ok ? APPROVED_KIND : CHANGES_KIND;
      doc.kinds[kind] ??= { versions: [] };
      const entry = doc.kinds[kind];
      if (entry.versions.some((v) => (JSON.parse(v.content) as { approval?: string }).approval === id)) continue;
      const note = ok ? `Approved version ${version}` : (a.verdict.reason ?? '');
      const content = JSON.stringify({ version, ...(artifact ? { artifact, hash } : {}), note, approval: id });
      const sealed: SealedArtifact = { kind, version: entry.versions.length + 1, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, outcome: `v${version}`, summary: note.slice(0, 80) };
      entry.versions.push(sealed);
    }
  }, ctx);
}

type Versions = Record<string, { versions: SealedArtifact[] } | undefined>;
const passed = (v: SealedArtifact) => (v.verdict as { passed?: boolean } | null)?.passed === true;

/** The spec the newest approval before artifact `before` approved. A record names its artifact; an older record without one is
 * matched as the last passing artifact of its spec version sealed by the time the approval was requested. */
export function approvedSpec(all: Versions, approvals: Record<string, Approval>, before = Infinity): SealedArtifact | undefined {
  const specs = (all[CONTRACT_KIND]?.versions ?? []).filter((v) => v.version < before && passed(v));
  for (const r of (all[APPROVED_KIND]?.versions ?? []).toReversed()) {
    const { version, artifact, approval } = JSON.parse(r.content) as Proposed & { approval?: string };
    const asked = (approval && approvals[approval]?.requestedAt) || Infinity;
    const hit = artifact ? specs.find((v) => v.version === artifact) : specs.findLast((v) => v.sealedAt <= asked && (parseSpec(v.content) as ContractSpec).version === version);
    if (hit) return hit;
  }
  return undefined;
}

const approval = gated(defineTool({
  name: 'propose_contract',
  description: 'propose_contract',
  parameters: obj({}, { open: true }),
  async execute() { return reply('approved'); },
}) as ToolRegistration);

type Outcome = { text: string; isError?: boolean };
const NOT_NEWEST: Outcome = { text: 'Only the newest sealed version can be put up for approval, once it passes its checks; write artifacts/spec.json and call submit_contract.', isError: true };
const textOf = (r: { output?: Array<{ type: string; text?: string }> }) => r.output?.map((p) => p.text ?? '').join('') ?? '';

/** Puts one sealed version before the person and waits: only the newest, only once it passed, never one already approved. The facts the
 * approver reads (lint findings, breaks against the approved version) are computed here in code, not taken from the agent's account. */
export async function proposeVersion(sealed: SealedArtifact, api: ToolExecutionApi, ctx: Context): Promise<Outcome> {
  await reconcile(api, ctx);
  const all = ((await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {}) as Versions;
  const spec = parseSpec(sealed.content);
  // Read back, not from `sealed`: the caller's copy may predate the verdict attached to it.
  const newest = all[CONTRACT_KIND]?.versions.at(-1);
  if (newest?.version !== sealed.version || !passed(newest) || typeof spec === 'string') return NOT_NEWEST;
  const base = approvedSpec(all, (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {});
  if (base?.version === sealed.version) return { text: `Version ${spec.version} is approved.` };
  const was = base && (parseSpec(base.content) as ContractSpec);
  if (was && was.version === spec.version) return { text: `Version ${spec.version} is already approved; write version ${spec.version + 1} to change it.`, isError: true };
  const kind = kinds[spec.kind];
  const [findings, breaks] = await Promise.all([kind.check(spec.document), was ? kind.diff(was.document, spec.document) : []]);
  const summary = `Approve ${routesOf(spec.document) || spec.title} v${spec.version}`;
  const verdict = await approval.execute({ summary, title: spec.title, version: spec.version, artifact: sealed.version, hash: sealed.hash, findings, breaks } as never, api, ctx);
  await reconcile(api, ctx);
  if (!verdict.isError) return { text: `Version ${spec.version} is approved.` };
  const text = textOf(verdict as never);
  if (!text.startsWith('Rejected by')) return { text, isError: true };
  return { text: `${text}. Act on what the person said, in this reason or in the message that follows it: for a change, write version ${spec.version + 1} with its changes and compatibility and call submit_contract; for a question, answer it and call submit_contract to put version ${spec.version} up again.`, isError: true };
}

/** The same proposal on demand, for the newest version. */
export const proposeContractTool: ToolRegistration = defineTool({
  name: 'propose_contract',
  description: 'Put the newest passing spec version up for the person to approve again, and wait for them. submit_contract already does this whenever a version passes.',
  parameters: obj({}, { open: true }),
  replay: 'safe',
  async execute(_args, api, ctx) {
    const newest = (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds[CONTRACT_KIND]?.versions.at(-1);
    const r = newest ? await proposeVersion(newest, api, ctx) : NOT_NEWEST;
    return reply(r.text, r.isError);
  },
}) as ToolRegistration;

/** A run may not end on a passed version that is neither approved nor up for approval: the person approves only what is put to them. */
export const awaitingApproval = hook(GenerationTask, {
  async onYield(_answer, api, ctx) {
    const all = ((await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {}) as Versions;
    const newest = all[CONTRACT_KIND]?.versions.at(-1);
    if (!newest || !passed(newest)) return undefined;
    if (approvedSpec(all, (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {})?.version === newest.version) return undefined;
    return { continue: `Version ${(parseSpec(newest.content) as ContractSpec).version} passed its checks but is not approved. Call submit_contract to put it to the person before you finish.` };
  },
});
