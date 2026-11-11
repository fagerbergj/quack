import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { Context } from '@earendil-works/chord/context';
import { defineDoc, defineExtension, wrapTool, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { createEditTool, createWriteTool } from '@earendil-works/pi-durable/tools';
import { assertSealFits } from './channel.ts';

/** An extension-declared artifact: a finished product at a workspace path, validated by its schema. */
export type ArtifactKind = {
  kind: string; path: string; validate?: (text: string) => string[];
  /** A one-word reading of the sealed content (a review's verdict), stored on the version so lists need not fetch content. */
  outcome?: (text: string) => string | undefined;
  /** One line saying what the content concluded, stored with the version so lists can show it without reading content; cut to SUMMARY_MAX. */
  summarize?: (content: string) => string;
  /** What the version was written against (a review's head commit), stored on it so a viewer can tell when it has been overtaken. */
  basis?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
};

export const SUMMARY_MAX = 80;

/** A pointer an extension attaches to a sealed version after the fact, e.g. where it was delivered. */
export type ArtifactLink = { label: string; url: string };

export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: JsonObject | null; links?: ArtifactLink[]; outcome?: string; summary?: string; basis?: string };

type ArtifactsDoc = { kinds: Record<string, { versions: SealedArtifact[] }> };

export const Artifacts = defineDoc<ArtifactsDoc>({
  kind: 'quack.artifacts',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ kinds: {} }),
  checkpointWhen: () => false,
});

/** The kind's one-line summary, cut to SUMMARY_MAX; a summarizer that throws or returns nothing leaves the version without one. */
export function summaryOf(kind: ArtifactKind, content: string): string | undefined {
  let line = '';
  try { line = kind.summarize?.(content).replace(/\s+/g, ' ').trim() ?? ''; } catch { /* a summary is never worth failing a seal */ }
  return !line ? undefined : line.length > SUMMARY_MAX ? `${line.slice(0, SUMMARY_MAX - 1).trimEnd()}…` : line;
}

const plain = <T>(value: T): T => JSON.parse(JSON.stringify(value));

async function readWorkspace(api: ToolExecutionApi, path: string, ctx: Context): Promise<string> {
  if (!api.env) throw new Error('this session has no workspace');
  const read = await api.env.readTextFile(path, ctx);
  if (!read.ok) throw new Error(`${path}: ${read.error.message ?? read.error.code ?? 'cannot be read'}`);
  return read.value;
}

// Seals the file as an immutable version in the log; an unchanged file keeps its version.
// Throws with every schema problem when the file does not validate.
export async function seal(api: ToolExecutionApi, kind: ArtifactKind, ctx: Context): Promise<SealedArtifact> {
  const content = await readWorkspace(api, kind.path, ctx);
  const problems = kind.validate?.(content) ?? [];
  if (problems.length) throw new Error(`${kind.path} does not match ${kind.kind}:\n- ${problems.join('\n- ')}`);
  const hash = createHash('sha256').update(content).digest('hex');
  const basis = await kind.basis?.(api, ctx);
  return api.commit(async (tx) => {
    const doc = await tx.doc(Artifacts, api.conversationId);
    // Read back through the draft: `??=` yields the plain object assigned, and mutations to it are lost.
    doc.kinds[kind.kind] ??= { versions: [] };
    const entry = doc.kinds[kind.kind];
    const last = entry.versions.at(-1);
    if (last?.hash === hash) return plain(last);
    const sealed: SealedArtifact = { kind: kind.kind, version: entry.versions.length + 1, hash, content, sealedAt: api.now(), verdict: null };
    const outcome = kind.outcome?.(content);
    if (outcome) sealed.outcome = outcome;
    const summary = summaryOf(kind, content);
    if (summary) sealed.summary = summary;
    if (basis) sealed.basis = basis;
    assertSealFits(kind.kind, sealed);
    entry.versions.push(sealed);
    return plain(sealed);
  }, ctx);
}

/** Attaches a checker's verdict to the sealed version it checked. */
export async function attachVerdict(api: ToolExecutionApi, kind: string, version: number, verdict: JsonObject, ctx: Context): Promise<void> {
  await api.commit(async (tx) => {
    const sealed = (await tx.doc(Artifacts, api.conversationId)).kinds[kind]?.versions[version - 1];
    if (!sealed) throw new Error(`${kind} v${version} is not sealed`);
    assertSealFits(kind, { ...plain(sealed), verdict: plain(verdict) });
    sealed.verdict = plain(verdict);
  }, ctx);
}

/** Appends a link to a sealed version; the content and hash stay as sealed, and a repeated URL is not added twice. */
export async function attachLink(api: ToolExecutionApi, kind: string, version: number, link: ArtifactLink, ctx: Context): Promise<void> {
  await api.commit(async (tx) => {
    const sealed = (await tx.doc(Artifacts, api.conversationId)).kinds[kind]?.versions[version - 1];
    if (!sealed) throw new Error(`${kind} v${version} is not sealed`);
    if (sealed.links?.some((l) => l.url === link.url)) return;
    assertSealFits(kind, { ...plain(sealed), links: [...(sealed.links ?? []), plain(link)] });
    sealed.links = [...(sealed.links ?? []), plain(link)];
  }, ctx);
}

/** A sealed version, read back for delivery by reference: what goes out is exactly what was checked. */
export async function sealedVersion(api: Pick<ToolExecutionApi, 'snapshot' | 'conversationId'>, kind: string, version: number, ctx: Context) {
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  return doc?.kinds[kind]?.versions[version - 1];
}

function validateAfter(tool: ToolRegistration, kinds: ArtifactKind[]): ToolRegistration {
  return {
    ...tool,
    async execute(args, api, ctx) {
      const result = await tool.execute(args, api, ctx);
      const kind = kinds.find((k) => k.path === (args as { path?: string }).path);
      if (!kind?.validate || result.isError) return result;
      const problems = kind.validate(await readWorkspace(api, kind.path, ctx));
      if (!problems.length) return result;
      const note = `${kind.path} does not yet match ${kind.kind}:\n- ${problems.join('\n- ')}`;
      return { ...result, output: [...(result.output ?? []), { type: 'text', text: note }] };
    },
  };
}

// Writes to a declared artifact path report every schema problem at once, without blocking the write.
export function artifactsExtension(kinds: ArtifactKind[]) {
  return defineExtension({
    name: 'quack.artifacts',
    wraps: [
      wrapTool(createWriteTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
      wrapTool(createEditTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
    ],
  });
}
