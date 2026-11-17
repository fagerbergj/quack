// AIP-158 paging for the app's lists: a pageSize of 1 to 1000, and a page token bound to the q that issued it.
import { createHash } from 'node:crypto';

// A pageSize or pageToken the list cannot read, including a token the catalog refuses: the 400.
export class BadRequest extends Error {}
export function sizeOf(param: string | null): number {
  const size = param === null ? 100 : /^\d+$/.test(param) ? Number(param) : 0;
  if (size < 1 || size > 1000) throw new BadRequest('pageSize must be an integer from 1 to 1000');
  return size;
}
// The page token is the catalog's cursor bound to the whole q, so a token sent with any other q is refused (AIP-158).
const digestOf = (q: string) => createHash('sha256').update(q.trim()).digest('base64url').slice(0, 22);
export const tokenOf = (cursor: string, q: string) => Buffer.from(JSON.stringify([cursor, digestOf(q)])).toString('base64url');
export function cursorOf(token: string | null, q: string): string {
  if (!token) return '';
  let parsed: unknown;
  try { parsed = JSON.parse(Buffer.from(token, 'base64url').toString()); } catch { parsed = undefined; }
  const [cursor, digest] = Array.isArray(parsed) ? parsed : [];
  if (typeof cursor !== 'string' || !cursor) throw new BadRequest('pageToken is not one this list issued');
  if (digest !== digestOf(q)) throw new BadRequest('pageToken was issued for a different q');
  return cursor;
}
