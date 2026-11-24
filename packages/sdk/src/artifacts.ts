import { createHash } from 'node:crypto';
import type { JsonObject } from '@earendil-works/pi-durable';
import type { Context } from '@earendil-works/chord/context';
import { obj } from '@earendil-works/pi-schema';
import { defineDoc, defineExtension, defineTool, LiveDoc, wrapTool, type ToolExecutionApi, type ToolExecutionResult, type ToolRegistration } from '@earendil-works/pi-durable';
import { createBashTool, createEditTool, createWriteTool } from '@earendil-works/pi-durable/tools';
import { assertSealFits } from './channel.ts';
import { runSealGates, SealRefused } from './gates.ts';

/** An extension-declared artifact: a finished product at a workspace path, validated by its schema. */
export type ArtifactKind = {
  kind: string; path: string; validate?: (text: string) => string[];
  /** A one-word reading of the sealed content (a review's verdict), stored on the version so lists need not fetch content. */
  outcome?: (text: string) => string | undefined;
  /** One line saying what the content concluded, stored with the version so lists can show it without reading content; cut to SUMMARY_MAX. */
  summarize?: (content: string) => string;
  /** What the version was written against (a review's head commit), stored on it so a viewer can tell when it has been overtaken. */
  basis?: (api: ToolExecutionApi, ctx: Context) => Promise<string | undefined>;
  /** The seal tool artifactsExtension installs for the kind (submit_review); a kind its extension's code seals has none. */
  tool?: string;
};

export const SUMMARY_MAX = 80;

/** A pointer an extension attaches to a sealed version after the fact, e.g. where it was delivered. */
export type ArtifactLink = { label: string; url: string };

export type SealedArtifact = {
  kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: JsonObject | null; links?: ArtifactLink[]; outcome?: string; summary?: string; basis?: string;
  /** Set by the seal tool on a judged version every gate passed: the run input it was delivered under, and the changed seals refused since. */
  final?: { input: string; refused: number };
};

/** written: the hash of the file as a write or edit last left it, and the run input it was written under. */
type ArtifactsDoc = { kinds: Record<string, { versions: SealedArtifact[]; written?: { hash: string; input: string } }> };

/** What the readers of a snapshot need of the artifacts doc; a session that sealed nothing has none. */
export type ArtifactsView = { kinds: Record<string, { versions: SealedArtifact[] } | undefined> } | null | undefined;

export const Artifacts = defineDoc<ArtifactsDoc>({
  kind: 'quack.artifacts',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ kinds: {} }),
  checkpointWhen: () => false,
});

/** Every version sealed of a kind, oldest first; none when the kind was never sealed. */
export const sealedVersions = (doc: ArtifactsView, kind: string): SealedArtifact[] => doc?.kinds[kind]?.versions ?? [];

/** The newest version sealed of a kind, or undefined. */
export const latestSealed = (doc: ArtifactsView, kind: string): SealedArtifact | undefined => sealedVersions(doc, kind).at(-1);

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

// A person's steer joins the running run as a new input, so the newest input tells a change request from the writer's own edit.
const newestInput = async (api: ToolExecutionApi, ctx: Context) => {
  const id = (await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs.at(-1);
  return id && String(id);
};

const sha256 = (content: string) => createHash('sha256').update(content).digest('hex');

/** Why the run may not finish: it changed the kind's file after the newest version and sealed nothing since; undefined when it may. */
export async function unsubmitted(api: Pick<ToolExecutionApi, 'snapshot' | 'conversationId'>, kind: ArtifactKind, ctx: Context): Promise<string | undefined> {
  const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
  const [last, written] = [latestSealed(doc, kind.kind), doc?.kinds[kind.kind]?.written];
  const inputs = (await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs.map(String) ?? [];
  // A change after a version final for this input is the seal's to refuse, not something to submit.
  if (!last || !written || written.hash === last.hash || !inputs.includes(written.input) || last.final?.input === inputs.at(-1)) return undefined;
  return `${kind.path} changed after ${kind.kind} v${last.version} and this run has not submitted it, so the change is not delivered: v${last.version} is still the newest version. Call ${kind.tool} to submit it before you finish.`;
}

const outcomeOf = (kind: ArtifactKind, last: SealedArtifact) => `${kind.kind} v${last.version} ${last.verdict?.passed ? 'passed and is delivered' : 'is delivered'}`;

// The first change after a final version is refused so the writer can still reply; a second ends the run, so a looping writer costs one turn.
async function refuseAfterFinal(api: ToolExecutionApi, kind: ArtifactKind, last: SealedArtifact, ctx: Context): Promise<never> {
  // A rerun of this call after a crash finds its refusal in the call's memo, so one refusal counts once.
  const key = `seal:final:${kind.kind}:v${last.version}`;
  let again = await api.memo<boolean>(key, ctx);
  if (again === undefined) {
    again = await api.memo(key, last.final!.refused > 0, ctx);
    await updateSealed(api, kind.kind, last.version, ctx, (sealed) => void sealed.final!.refused++);
  }
  const said = again
    ? `${outcomeOf(kind, last)}. A second change after it was not sealed either, so this run is over.`
    : `${outcomeOf(kind, last)}, so it is final for this run and this change to ${kind.path} was not sealed. Finish with your reply and seal nothing more; only a person's change request reopens it.`;
  throw new SealRefused(again ? { kind: 'deny', reason: said } : { kind: 'continue', feedback: said }, said);
}

// An unchanged file keeps its version but reruns the gates, so a failed delivery is retried; a deny or continue rejects with SealRefused,
// as does a change after a final version. Throws with every schema problem when the file does not validate.
export async function seal(api: ToolExecutionApi, kind: ArtifactKind, ctx: Context): Promise<{ artifact: SealedArtifact; notes: string[] }> {
  const content = await readWorkspace(api, kind.path, ctx);
  const problems = kind.validate?.(content) ?? [];
  if (problems.length) throw new Error(`${kind.path} does not match ${kind.kind}:\n- ${problems.join('\n- ')}`);
  const hash = sha256(content);
  const [basis, input] = [await kind.basis?.(api, ctx), await newestInput(api, ctx)];
  const newest = latestSealed(await api.snapshot(Artifacts, api.conversationId, ctx), kind.kind);
  if (input && newest?.final?.input === input && newest.hash !== hash) await refuseAfterFinal(api, kind, newest, ctx);
  const artifact = await api.commit(async (tx) => {
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
  return { artifact, notes: await runSealGates(api, artifact, ctx) };
}

// Changes one sealed version in place; throws when the version is not sealed.
async function updateSealed(api: ToolExecutionApi, kind: string, version: number, ctx: Context, change: (sealed: SealedArtifact) => void): Promise<void> {
  await api.commit(async (tx) => {
    const sealed = (await tx.doc(Artifacts, api.conversationId)).kinds[kind]?.versions[version - 1];
    if (!sealed) throw new Error(`${kind} v${version} is not sealed`);
    change(sealed);
  }, ctx);
}

/** Attaches a checker's verdict to the sealed version it checked. */
export async function attachVerdict(api: ToolExecutionApi, kind: string, version: number, verdict: JsonObject, ctx: Context): Promise<void> {
  await updateSealed(api, kind, version, ctx, (sealed) => {
    assertSealFits(kind, { ...plain(sealed), verdict: plain(verdict) });
    sealed.verdict = plain(verdict);
  });
}

/** Appends a link to a sealed version; the content and hash stay as sealed, and a repeated URL is not added twice. */
export async function attachLink(api: ToolExecutionApi, kind: string, version: number, link: ArtifactLink, ctx: Context): Promise<void> {
  await updateSealed(api, kind, version, ctx, (sealed) => {
    if (sealed.links?.some((l) => l.url === link.url)) return;
    assertSealFits(kind, { ...plain(sealed), links: [...(sealed.links ?? []), plain(link)] });
    sealed.links = [...(sealed.links ?? []), plain(link)];
  });
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
      if (!kind || result.isError) return result;
      const content = await readWorkspace(api, kind.path, ctx);
      const written = { hash: sha256(content), input: (await newestInput(api, ctx)) ?? '' };
      await api.commit(async (tx) => {
        const doc = await tx.doc(Artifacts, api.conversationId);
        doc.kinds[kind.kind] ??= { versions: [] };
        doc.kinds[kind.kind].written = written;
      }, ctx);
      const problems = kind.validate?.(content) ?? [];
      if (!problems.length) return result;
      const note = `${kind.path} does not yet match ${kind.kind}:\n- ${problems.join('\n- ')}`;
      return { ...result, output: [...(result.output ?? []), { type: 'text', text: note }] };
    },
  };
}

// Every gate passed a version a checker judged, so the writer is done with it; code that seals (a workflow) decides for itself.
async function markFinal(api: ToolExecutionApi, kind: ArtifactKind, version: number, ctx: Context): Promise<void> {
  const input = await newestInput(api, ctx);
  if (!input || !(await sealedVersion(api, kind.kind, version, ctx))?.verdict) return;
  await updateSealed(api, kind.kind, version, ctx, (sealed) => void (sealed.final = sealed.final?.input === input ? sealed.final : { input, refused: 0 }));
}

const text = (t: string, isError = false): ToolExecutionResult => ({ output: [{ type: 'text', text: t }], ...(isError && { isError }) });

// A deny ends the run after this round; a continue or an invalid file is an error the model acts on.
function sealTool(kind: ArtifactKind & { tool: string }): ToolRegistration {
  return defineTool({
    name: kind.tool,
    description: `Seal ${kind.path} as the next version of the ${kind.kind}; any checks run before the version is accepted. Call it when the ${kind.kind} is ready.`,
    parameters: obj({}, { open: true }),
    replay: 'safe',
    async execute(_args, api, ctx) {
      try {
        const { artifact, notes } = await seal(api, kind, ctx);
        await markFinal(api, kind, artifact.version, ctx);
        return text(notes.length ? notes.join('\n') : `${kind.kind} v${artifact.version} sealed.`);
      } catch (err) {
        if (err instanceof SealRefused && err.verdict.kind === 'deny') return { ...text(err.message, true), control: { terminate: true } };
        return text((err as Error).message, true);
      }
    },
  }) as ToolRegistration;
}

// One process owns one session (see signals.ts), so the kinds its extensions declare are module state; a later declaration wins.
const declared = new Map<string, ArtifactKind>();

/** The kind a session's artifactsExtension declared under this name, so other extensions (the judge) can read its path and tool. */
export const artifactKind = (name: string): ArtifactKind | undefined => declared.get(name);

// Shell commands can edit an artifact file without write/edit; record the new hash so unsubmitted() catches it.
function trackBashWrites(tool: ToolRegistration, kinds: ArtifactKind[]): ToolRegistration {
  return {
    ...tool,
    async execute(args, api, ctx) {
      const result = await tool.execute(args, api, ctx);
      if (result.isError || !api.env) return result;
      const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
      const input = (await newestInput(api, ctx)) ?? '';
      const changed: Array<{ kind: string; hash: string }> = [];
      for (const k of kinds) {
        const read = await api.env.readTextFile(k.path, ctx);
        if (!read.ok) continue;
        const hash = sha256(read.value);
        const entry = doc?.kinds[k.kind];
        if (hash !== entry?.written?.hash && hash !== latestSealed(doc, k.kind)?.hash) changed.push({ kind: k.kind, hash });
      }
      if (changed.length) {
        await api.commit(async (tx) => {
          const draft = await tx.doc(Artifacts, api.conversationId);
          for (const { kind, hash } of changed) {
            draft.kinds[kind] ??= { versions: [] };
            draft.kinds[kind].written = { hash, input };
          }
        }, ctx);
      }
      return result;
    },
  };
}

// Writes to a declared artifact path report every schema problem at once, without blocking the write; a kind naming a tool gets its seal tool.
export function artifactsExtension(kinds: ArtifactKind[]) {
  for (const k of kinds) declared.set(k.kind, k);
  return defineExtension({
    name: 'quack.artifacts',
    tools: kinds.filter((k): k is ArtifactKind & { tool: string } => !!k.tool).map(sealTool),
    wraps: [
      wrapTool(createWriteTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
      wrapTool(createEditTool() as ToolRegistration, (t) => validateAfter(t, kinds)),
      wrapTool(createBashTool() as ToolRegistration, (t) => trackBashWrites(t, kinds)),
    ],
  });
}
