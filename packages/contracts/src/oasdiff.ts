import { execFile } from 'node:child_process';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';

export type Break = { id: string; text: string; operation: string; path: string };

// The guide counts removing an optional response field as breaking; oasdiff reports it as info unless raised.
const SEVERITY = 'response-optional-property-removed err\n';

/** Breaking changes between two OpenAPI documents, from oasdiff (QUACK_OASDIFF names the binary; the image carries one). */
export async function breakingChanges(prev: unknown, next: unknown, bin = process.env.QUACK_OASDIFF ?? 'oasdiff'): Promise<Break[]> {
  const dir = await mkdtemp(join(tmpdir(), 'oasdiff-'));
  try {
    const [a, b, levels] = ['prev.json', 'next.json', 'levels.txt'].map((f) => join(dir, f));
    await Promise.all([writeFile(a, JSON.stringify(prev)), writeFile(b, JSON.stringify(next)), writeFile(levels, SEVERITY)]);
    const { stdout } = await promisify(execFile)(bin, ['breaking', a, b, '--format', 'json', '--severity-levels', levels]);
    const found = JSON.parse(stdout || '[]') as Array<Break & { level: number }>;
    return found.filter((c) => c.level >= 2).map(({ id, text, operation, path }) => ({ id, text, operation, path }));
  } catch (err) {
    throw new Error(`oasdiff failed (${bin}): ${(err as Error).message.split('\n')[0]}`);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}
