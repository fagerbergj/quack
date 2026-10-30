import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { Context } from '@earendil-works/chord/context';
import { defineDoc, defineExtension, wrapTool, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { createEditTool, createWriteTool } from '@earendil-works/pi-durable/tools';

/** An extension-declared artifact: a finished product at a workspace path, validated by its schema (D42). */
export type ArtifactKind = { kind: string; path: string; validate?: (text: string) => string[] };

export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: JsonObject | null };

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

const plain = <T>(value: T): T => JSON.parse(JSON.stringify(value));

async function readWorkspace(api: ToolExecutionApi, path: string, ctx: Context): Promise<string> {
  if (!api.env) throw new Error('this session has no workspace');
  const read = await api.env.readTextFile(path, ctx);
  if (!read.ok) throw new Error(`${path}: ${read.error.message ?? read.error.code ?? 'cannot be read'}`);
  return read.value;
}

/**
 * Seals the artifact's current file as an immutable version in the log (ART-3); an unchanged file returns its existing version.
 * Throws with every schema problem when the file does not validate.
 */
export async function seal(api: ToolExecutionApi, kind: ArtifactKind, ctx: Context): Promise<SealedArtifact> {
  const content = await readWorkspace(api, kind.path, ctx);
  const problems = kind.validate?.(content) ?? [];
  if (problems.length) throw new Error(`${kind.path} does not match ${kind.kind}:\n- ${problems.join('\n- ')}`);
  const hash = createHash('sha256').update(content).digest('hex');
  return api.commit(async (tx) => {
    const doc = await tx.doc(Artifacts, api.conversationId);
    const entry = (doc.kinds[kind.kind] ??= { versions: [] });
    const last = entry.versions.at(-1);
    if (last?.hash === hash) return plain(last);
    const sealed: SealedArtifact = { kind: kind.kind, version: entry.versions.length + 1, hash, content, sealedAt: api.now(), verdict: null };
    entry.versions.push(sealed);
    return plain(sealed);
  }, ctx);
}

/** Attaches a checker's verdict to the sealed version it checked. */
export async function attachVerdict(api: ToolExecutionApi, kind: string, version: number, verdict: JsonObject, ctx: Context): Promise<void> {
  await api.commit(async (tx) => {
    const sealed = (await tx.doc(Artifacts, api.conversationId)).kinds[kind]?.versions[version - 1];
    if (!sealed) throw new Error(`${kind} v${version} is not sealed`);
    sealed.verdict = plain(verdict);
  }, ctx);
}

/** A sealed version, read back for delivery by reference (ART-4): what goes out is exactly what was checked. */
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

// Writes to a declared artifact path report every schema problem at once (ART-2), without blocking the write.
export function artifactsExtension(kinds: ArtifactKind[]) {
  return defineExtension({
    name: 'quack.artifacts',
    wraps: [
      wrapTool(createWriteTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
      wrapTool(createEditTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
    ],
  });
}
