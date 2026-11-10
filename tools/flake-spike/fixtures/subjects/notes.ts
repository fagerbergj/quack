import { readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

/** Saves a note as <slug>.md in dir and returns how many notes the dir now holds. */
export function saveNote(dir: string, slug: string, text: string): number {
  writeFileSync(join(dir, `${slug}.md`), text);
  return readdirSync(dir).filter((f) => f.endsWith('.md')).length;
}
