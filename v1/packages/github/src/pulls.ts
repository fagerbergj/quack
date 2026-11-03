import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type { GithubApp } from './app.ts';

export type PrState = { state: 'open' | 'merged' | 'closed'; at: string };
type Entry = PrState & { checked: number };

const OPEN_TTL_MS = 10 * 60_000;
const MAX_KEYS = 100;
const KEY = /^(?:github:)?([^/\s,]+)\/([^#\s,]+)#(\d+)$/;

export const prStateKey = (owner: string, repo: string, number: number) => `${owner.toLowerCase()}/${repo.toLowerCase()}#${number}`;

export const stateOf = (pr: { merged?: boolean; state?: string }): PrState['state'] => (pr.merged ? 'merged' : pr.state === 'closed' ? 'closed' : 'open');

/** PR states the webhooks reported, kept across restarts when given a file; merged and closed are final, open is re-read after a TTL. */
export function pullStates(file?: string, now: () => number = Date.now) {
  let entries: { [key: string]: Entry } = {};
  try {
    if (file) entries = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    // First run or an unreadable file: states are re-fetched lazily.
  }
  const save = () => {
    if (!file) return;
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(`${file}.tmp`, JSON.stringify(entries));
    renameSync(`${file}.tmp`, file);
  };
  const fresh = (e: Entry | undefined) => e && (e.state !== 'open' || now() - e.checked < OPEN_TTL_MS);
  async function read(app: GithubApp, asked: string): Promise<PrState | undefined> {
    const m = KEY.exec(asked);
    if (!m) return undefined;
    const key = prStateKey(m[1], m[2], Number(m[3]));
    if (!fresh(entries[key])) {
      try {
        const pr = (await app.asInstallation(m[1], m[2], 'GET', `/repos/${m[1]}/${m[2]}/pulls/${m[3]}`)).json;
        if (pr) entries[key] = { state: stateOf(pr), at: pr.merged_at ?? pr.closed_at ?? pr.updated_at ?? new Date(now()).toISOString(), checked: now() };
      } catch {
        // Not installed or GitHub down: the list shows no state rather than an error.
      }
    }
    const e = entries[key];
    return e && { state: e.state, at: e.at };
  }
  return {
    record(key: string, state: PrState['state'], at: string) {
      entries[key] = { state, at, checked: now() };
      save();
    },
    /** States for `owner/repo#n` keys (optionally `github:`-prefixed); keys with no installation or a failed read are left out. */
    async lookup(app: GithubApp, keys: string[]): Promise<{ [key: string]: PrState }> {
      const asked = keys.slice(0, MAX_KEYS);
      const found = await Promise.all(asked.map((k) => read(app, k)));
      save();
      return Object.fromEntries(asked.flatMap((k, i) => (found[i] ? [[k.replace(/^github:/, ''), found[i]]] : [])));
    },
  };
}
