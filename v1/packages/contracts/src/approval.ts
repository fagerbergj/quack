import { createHash } from 'node:crypto';
import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineTool, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { Approvals, Artifacts, gated, type SealedArtifact } from '@quack/sdk';
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

// Writes the artifact for every decided proposal that has none, keyed by approval id, so a crash between the verdict and the record loses nothing.
async function reconcile(api: ToolExecutionApi, ctx: Context) {
  await api.commit(async (tx) => {
    const { items } = await tx.doc(Approvals, api.conversationId);
    const doc = await tx.doc(Artifacts, api.conversationId);
    for (const [id, a] of Object.entries(items)) {
      if (a.tool !== 'propose_contract' || !a.verdict) continue;
      const args = a.args as { version: number };
      const ok = a.verdict.decision === 'approve';
      const kind = ok ? APPROVED_KIND : CHANGES_KIND;
      doc.kinds[kind] ??= { versions: [] };
      const entry = doc.kinds[kind];
      if (entry.versions.some((v) => (JSON.parse(v.content) as { approval?: string }).approval === id)) continue;
      const note = ok ? `Approved version ${args.version}` : (a.verdict.reason ?? '');
      const content = JSON.stringify({ version: args.version, note, approval: id });
      const sealed: SealedArtifact = { kind, version: entry.versions.length + 1, hash: createHash('sha256').update(content).digest('hex'), content, sealedAt: api.now(), verdict: null, outcome: `v${args.version}`, summary: note.slice(0, 80) };
      entry.versions.push(sealed);
    }
  }, ctx);
}

type Versions = Record<string, { versions: SealedArtifact[] }>;

// The facts the approver reads are computed here in code, not taken from the agent's account of them.
async function evidence(api: ToolExecutionApi, ctx: Context) {
  const all = ((await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {}) as Versions;
  const specs = (all[CONTRACT_KIND]?.versions ?? []).map((v) => ({ v, spec: parseSpec(v.content) }));
  const latest = specs.filter((s) => (s.v.verdict as { passed?: boolean } | null)?.passed).at(-1)?.spec;
  if (!latest || typeof latest === 'string') return undefined;
  const approved = all[APPROVED_KIND]?.versions.at(-1);
  const was = approved && (JSON.parse(approved.content) as { version: number }).version;
  const prev = specs.map((s) => s.spec).find((s): s is ContractSpec => typeof s !== 'string' && s.version === was);
  const kind = kinds[latest.kind];
  return { spec: latest, approvedVersion: was, findings: await kind.check(latest.document), breaks: prev ? await kind.diff(prev.document, latest.document) : [] };
}

const approval = gated(defineTool({
  name: 'propose_contract',
  description: 'propose_contract',
  parameters: obj({}, { open: true }),
  async execute() { return reply('approved'); },
}) as ToolRegistration);

/** Puts the latest passing spec version before the person: approved, it is recorded for the Contracts page; rejected, the reason comes back as a change request. */
export const proposeContractTool: ToolRegistration = defineTool({
  name: 'propose_contract',
  description: 'Ask the person to approve the latest sealed spec version, once they have no more changes. It waits for them. Approved, the version is recorded; rejected, you get their reason as a change request to apply as the next version.',
  parameters: obj({}, { open: true }),
  replay: 'safe',
  async execute(_args, api, ctx) {
    await reconcile(api, ctx);
    const found = await evidence(api, ctx);
    if (!found) return reply('No sealed spec version has passed its checks; write artifacts/spec.json and call submit_contract first.', true);
    const { spec, findings, breaks, approvedVersion } = found;
    if (approvedVersion === spec.version) return reply(`Version ${spec.version} is already approved; write version ${spec.version + 1} to change it.`, true);
    const summary = `Approve ${routesOf(spec.document) || spec.title} v${spec.version}`;
    const verdict = await approval.execute({ summary, title: spec.title, version: spec.version, findings, breaks } as never, api, ctx);
    const text = verdict.output?.map((p) => ('text' in p ? p.text : '')).join('') ?? '';
    await reconcile(api, ctx);
    if (!verdict.isError) {
      return reply(`Version ${spec.version} is approved.`);
    }
    if (!text.startsWith('Rejected by')) return verdict;
    return reply(`${text}. Treat the reason as a change request: ask about anything it leaves open, write version ${spec.version + 1} with its changes and compatibility, call submit_contract, then propose_contract again.`, true);
  },
}) as ToolRegistration;
