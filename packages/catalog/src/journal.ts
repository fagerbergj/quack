import { createHash } from 'node:crypto';
import { mkdir, open, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { Json } from './port.ts';

/** The action writes the core journals, by the port method each one replays through. */
export type ActionMethod = 'setTitle' | 'archiveSession' | 'unarchiveSession' | 'resolveAsk' | 'deleteSession' | 'transitionSession' | 'updateArtifactVerdict' | 'addArtifactLink';

/** One journal line: an action write with the channel watermark at its apply, or the incarnation a session.created began. */
export type JournalLine = { id: string; method: ActionMethod; args: { [field: string]: Json }; watermark: number } | { incarnation: string };

export type Journal = {
  append(key: string, line: JournalLine): Promise<void>;
  read(key: string): Promise<JournalLine[]>;
};

/** One append-only file per session in a directory of the distributor's own, outside every session mount. */
export function fileJournal(dir: string): Journal {
  const fileOf = (key: string) => join(dir, `${createHash('sha256').update(key).digest('hex').slice(0, 40)}.jsonl`);
  let made: Promise<unknown> | undefined;
  return {
    async append(key, line) {
      await (made ??= mkdir(dir, { recursive: true }));
      const file = await open(fileOf(key), 'a');
      try {
        await file.write(`${JSON.stringify(line)}\n`);
        // The line must outlive a crash that the write it records survives.
        await file.datasync();
      } finally {
        await file.close();
      }
    },
    async read(key) {
      const text = await readFile(fileOf(key), 'utf8').catch((err) => (err.code === 'ENOENT' ? '' : Promise.reject(err)));
      const lines = text.split('\n').filter(Boolean);
      // A crash mid-append leaves a torn last line, which never applied.
      return lines.flatMap((l, i) => {
        try {
          return [JSON.parse(l) as JournalLine];
        } catch (err) {
          if (i === lines.length - 1) return [];
          throw err;
        }
      });
    },
  };
}
