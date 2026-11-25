import { createHash } from 'node:crypto';
import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineTool, GenerationTask, hook, LiveDoc, type ConversationId, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, gated, latestSealed, sealedVersions, withdrawApprovals, type Approval, type ArtifactsView, type SealedArtifact } from '@quack/sdk';
import { kinds } from './kinds.ts';
import { lastReviewBlock, reasonSummary } from './reason.ts';
import { CONTRACT_KIND, CONTRACT_PATH, parseSpec, type ContractSpec } from './spec.ts';

export const APPROVED_KIND = 'contract.approved';
export const CHANGES_KIND = 'contract.changes';
export const DISCARDED_KIND = 'contract.discarded';

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
// A withdrawn proposal's verdict never acted, so it is not recorded; a record written before the withdrawal stands.
async function reconcile(api: ToolExecutionApi, ctx: Context) {
  await api.commit(async (tx) => {
    const { items } = await tx.doc(Approvals, api.conversationId);
    const doc = await tx.doc(Artifacts, api.conversationId);
    for (const [id, a] of Object.entries(items)) {
      if (a.tool !== 'propose_contract' || !a.verdict || a.withdrawn) continue;
      const { version, artifact, hash } = a.args as Proposed;
      const ok = a.verdict.decision === 'approve';
      const kind = ok ? APPROVED_KIND : CHANGES_KIND;
      doc.kinds[kind] ??= { versions: [] };
      const entry = doc.kinds[kind];
      if (entry.versions.some((v) => (JSON.parse(v.content) as { approval?: string }).approval === id)) continue;
      const note = ok ? `Approved version ${version}` : (a.verdict.reason ?? '');
      const content = JSON.stringify({ version, ...(artifact ? { artifact, hash } : {}), note, approval: id });
      const sealed: SealedArtifact = { kind, version: entry.versions.length + 1, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, outcome: `v${version}`, summary: reasonSummary(note).slice(0, 80) };
      entry.versions.push(sealed);
    }
  }, ctx);
}

const passed = (v: SealedArtifact) => (v.verdict as { passed?: boolean } | null)?.passed === true;

/** The spec the newest approval before artifact `before` approved. A record names its artifact; an older record without one is
 * matched as the last passing artifact of its spec version sealed by the time the approval was requested. */
export function approvedSpec(all: NonNullable<ArtifactsView>['kinds'], approvals: Record<string, Approval>, before = Infinity): SealedArtifact | undefined {
  const doc = { kinds: all };
  const specs = sealedVersions(doc, CONTRACT_KIND).filter((v) => v.version < before && passed(v));
  const recorded = sealedVersions(doc, APPROVED_KIND).map((r) => JSON.parse(r.content) as Proposed & { approval?: string });
  // A decided approval not yet written down by `reconcile` counts too, and outranks recorded ones: it was decided after the last reconcile.
  const pending = Object.entries(approvals).filter(([id, a]) => a.tool === 'propose_contract' && a.verdict?.decision === 'approve' && !a.withdrawn && !recorded.some((r) => r.approval === id));
  for (const { version, artifact, approval } of [...recorded, ...pending.map(([approval, a]) => ({ ...(a.args as Proposed), approval }))].toReversed()) {
    const asked = (approval && approvals[approval]?.requestedAt) || Infinity;
    const hit = artifact ? specs.find((v) => v.version === artifact) : specs.findLast((v) => v.sealedAt <= asked && (parseSpec(v.content) as ContractSpec).version === version);
    if (hit) return hit;
  }
  return undefined;
}

/** Withdraws every open proposal of an artifact older than the newly sealed `version`: approving it would approve a replaced version. */
export async function supersedeProposals(version: number, api: ToolExecutionApi, ctx: Context): Promise<void> {
  const older = (a: Approval) => a.tool === 'propose_contract' && ((a.args as Proposed).artifact ?? Infinity) < version;
  await api.commit(async (tx) => { withdrawApprovals(await tx.doc(Approvals, api.conversationId), (_, a) => older(a), { at: api.now(), reason: 'superseded', supersededBy: `${CONTRACT_KIND}:${version}` }); }, ctx);
}

const approval = gated(defineTool({
  name: 'propose_contract',
  description: 'propose_contract',
  parameters: obj({}, { open: true }),
  async execute() { return reply('approved'); },
}) as ToolRegistration, { subject: (args) => ({ kind: CONTRACT_KIND, version: (args as Proposed).artifact! }) });

type Outcome = { text: string; isError?: boolean };
// How the gate opens a rejection the approve-time checks made.
const CHECKED = 'Not approved: the checks found problems';
const NOT_NEWEST: Outcome = { text: 'Only the newest sealed version can be put up for approval, once it passes its checks; write artifacts/spec.json and call submit_contract.', isError: true };
const textOf = (r: { output?: Array<{ type: string; text?: string }> }) => r.output?.map((p) => p.text ?? '').join('') ?? '';

// What the agent is told after a rejection; a review's block ends the reason, so what follows starts on a line of its own.
function rejectedText(text: string, version: number, was: ContractSpec | undefined): Outcome {
  const against = was ? `with baseline ${was.version} and its changes and compatibility measured against that approved version` : 'with changes and compatibility only if its baseline is "shipped", never measured against this rejected version';
  // The gate says "Rejected by <who>: <reason>", and a reason with no note opens with its block; the checks' rejection carries no review.
  const review = !!lastReviewBlock(text.replace(/^Rejected by [^:\n]*: /, ''));
  const said = text.startsWith(CHECKED) ? 'what the checks found' : 'what the person said, in this reason or in the message that follows it';
  const comments = review ? ' The reason ends in a ```quack-review block of review comments: for each comment, name its id in the addresses of the changes row that changes what it is about, or call reply_comment with its id, before you submit, to say why you left it; the check fails a version that does neither for any of them.' : '';
  const drop = was ? `; if the person wants to drop this version and keep version ${was.version}, call discard_draft` : '';
  return { text: `${text}${review ? '\n\n' : '. '}Act on ${said}: for a change, write version ${version + 1} ${against} and call submit_contract; for a question, answer it and call submit_contract to put version ${version} up again${drop}.${comments}`, isError: true };
}

/** Puts one sealed version before the person and waits: only the newest, only once it passed, never one already approved. The facts the
 * approver reads (lint findings, breaks against the approved version) are computed here in code, not taken from the agent's account. */
export async function proposeVersion(sealed: SealedArtifact, api: ToolExecutionApi, ctx: Context): Promise<Outcome> {
  await reconcile(api, ctx);
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  const spec = parseSpec(sealed.content);
  // Read back, not from `sealed`: the caller's copy may predate the verdict attached to it.
  const newest = latestSealed(doc, CONTRACT_KIND);
  if (newest?.version !== sealed.version || !passed(newest) || typeof spec === 'string') return NOT_NEWEST;
  const base = approvedSpec(doc?.kinds ?? {}, (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {});
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
  if (!text.startsWith('Rejected by') && !text.startsWith(CHECKED)) return { text, isError: true };
  return rejectedText(text, spec.version, was);
}

/** The same proposal on demand, for the newest version. */
export const proposeContractTool: ToolRegistration = defineTool({
  name: 'propose_contract',
  description: 'Put the newest passing spec version up for the person to approve again, and wait for them. submit_contract already does this whenever a version passes.',
  parameters: obj({}, { open: true }),
  replay: 'safe',
  async execute(_args, api, ctx) {
    const newest = latestSealed(await api.snapshot(Artifacts, api.conversationId, ctx), CONTRACT_KIND);
    const r = newest ? await proposeVersion(newest, api, ctx) : NOT_NEWEST;
    return reply(r.text, r.isError);
  },
}) as ToolRegistration;

const isDiscarded = (doc: ArtifactsView, artifact: number) =>
  sealedVersions(doc, DISCARDED_KIND).some((v) => (JSON.parse(v.content) as { artifact?: number }).artifact === artifact);

/** Drops an unwanted unapproved draft, restoring the last approved spec to the workspace and satisfying the yield guard. */
export const discardDraftTool: ToolRegistration = defineTool({
  name: 'discard_draft',
  description: 'Discard an unwanted unapproved draft and restore the last approved contract version as the current one.',
  parameters: obj({}, { open: true }),
  replay: 'safe',
  async execute(_args, api, ctx) {
    await reconcile(api, ctx);
    const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
    const approvals = (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {};
    const base = approvedSpec(doc?.kinds ?? {}, approvals);
    if (!base) return reply('No approved version to return to.', true);
    const baseSpec = parseSpec(base.content) as ContractSpec;
    const newest = latestSealed(doc, CONTRACT_KIND);
    if (!newest || newest.version === base.version || isDiscarded(doc, newest.version)) return reply(`Version ${baseSpec.version} is already the current version.`);
    await api.env?.writeFile(CONTRACT_PATH, base.content, ctx);
    await supersedeProposals(Infinity, api, ctx);
    const content = JSON.stringify({ artifact: newest.version, restored: base.version, version: baseSpec.version });
    await api.commit(async (tx) => {
      const doc = await tx.doc(Artifacts, api.conversationId);
      // Clear any tracked edit on the discarded draft so unsubmitted() does not send the run back.
      if (doc.kinds[CONTRACT_KIND]) delete doc.kinds[CONTRACT_KIND].written;
      doc.kinds[DISCARDED_KIND] ??= { versions: [] };
      const entry = doc.kinds[DISCARDED_KIND];
      entry.versions.push({ kind: DISCARDED_KIND, version: entry.versions.length + 1, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, outcome: `v${baseSpec.version}`, summary: `Restored v${baseSpec.version}` });
    }, ctx);
    return reply(`Discarded unapproved draft; version ${baseSpec.version} stands as the current version.`);
  },
}) as ToolRegistration;

/** A run may not end on a passed version that is neither approved nor up for approval: the person approves only what is put to them.
 * It asks once per run and proposal, so an agent that answers again without putting the version up ends its run instead of looping. */
export function awaitingApproval() {
  // In memory like the cut-off streak: a restart only asks once more.
  const asked = new Map<ConversationId, string>();
  return hook(GenerationTask, {
    async onYield(_answer, api, ctx) {
      const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
      const newest = latestSealed(doc, CONTRACT_KIND);
      if (!newest || !passed(newest) || isDiscarded(doc, newest.version)) return undefined;
      const approvals = (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {};
      if (approvedSpec(doc?.kinds ?? {}, approvals)?.version === newest.version) return undefined;
      const run = (await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0];
      const proposals = Object.values(approvals).filter((a) => a.tool === 'propose_contract' && (a.args as Proposed).artifact === newest.version).length;
      const key = `${String(run)}:${newest.version}:${proposals}`;
      if (asked.get(api.conversationId) === key) return undefined;
      asked.set(api.conversationId, key);
      return { continue: `Version ${(parseSpec(newest.content) as ContractSpec).version} passed its checks but is not approved. Call submit_contract to put it to the person before you finish.` };
    },
  });
}
