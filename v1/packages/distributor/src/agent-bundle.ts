import { cp, mkdir, readFile, stat, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

export type AgentDetail = { name: string; version: number; status: 'draft' | 'promoted'; definition: Record<string, unknown>; prompt: string; rubric?: unknown };

/** The rubric an agent runs with, by the extension names its definition selects; undefined when none is exposed. */
export type RubricFor = (extensions: string[]) => unknown;

/** Agent names become directory names, so only plain ones are accepted. */
export const SAFE_NAME = /^[\w-][\w.-]*$/;
export const STORE = 'agent-versions';

const text = (path: string) => readFile(path, 'utf8').catch(() => undefined);
const exists = (path: string) => stat(path).then(() => true, () => false);

/** Reads a bundle directory: its definition, prompt, stored rubric and promotion marker; undefined when it is not a bundle. */
export async function readBundle(dir: string): Promise<AgentDetail | undefined> {
  const raw = await text(join(dir, 'agent.json'));
  const def = raw === undefined ? null : (JSON.parse(raw) as Record<string, unknown> | null);
  if (typeof def?.name !== 'string' || typeof def.version !== 'number' || !SAFE_NAME.test(def.name)) return undefined;
  const rubric = await text(join(dir, 'rubric.json'));
  const promoted = def.status === 'promoted' || (await exists(join(dir, 'promoted')));
  const prompt = typeof def.prompt === 'string' ? await text(join(dir, def.prompt)) : undefined;
  return { name: def.name, version: def.version, status: promoted ? 'promoted' : 'draft', definition: def, prompt: prompt ?? '', ...(rubric && { rubric: JSON.parse(rubric) }) };
}

export const versionDir = (dataDir: string, name: string, version: number) => join(dataDir, STORE, name, String(version));

/** Copies a bundle version the store has not seen: definition, prompt and its rubric. A stored version is never overwritten, and agent.json goes last so a half copy is retried. */
export async function keepVersion(dataDir: string, srcDir: string, rubricFor?: RubricFor): Promise<void> {
  const b = await readBundle(srcDir);
  if (!b) return;
  const dest = versionDir(dataDir, b.name, b.version);
  if (await exists(join(dest, 'agent.json'))) return;
  await mkdir(dest, { recursive: true });
  if (typeof b.definition.prompt === 'string') await cp(join(srcDir, b.definition.prompt), join(dest, b.definition.prompt)).catch(() => undefined);
  const rubric = rubricFor?.(Object.keys((b.definition.extensions as object | undefined) ?? {}));
  if (rubric) await writeFile(join(dest, 'rubric.json'), JSON.stringify(rubric));
  await cp(join(srcDir, 'agent.json'), join(dest, 'agent.json'));
}
