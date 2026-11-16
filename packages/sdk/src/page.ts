/** A pageToken or pageSize a service cannot accept; the service maps it to its own 400 class. */
export class PageError extends Error {}

const MAX_PAGE_SIZE = 1000;

/** The AIP-132 cursor of one list: the token is base64url of [list, value], and only a token that re-encodes to itself decodes. */
export function pageCursor(list: string) {
  const encode = (value: string) => Buffer.from(JSON.stringify([list, value])).toString('base64url');
  return {
    encode,
    decode(token: string | null): string | undefined {
      if (!token) return undefined;
      try {
        const [tag, value] = JSON.parse(Buffer.from(token, 'base64url').toString()) as [unknown, unknown];
        if (tag === list && typeof value === 'string' && encode(value) === token) return value;
      } catch { /* falls through */ }
      throw new PageError('pageToken is not one this service issued');
    },
    pageSize(param: string | null): number {
      const n = param === null ? 100 : /^\d+$/.test(param) ? Number(param) : 0;
      if (n < 1 || n > MAX_PAGE_SIZE) throw new PageError(`pageSize must be an integer from 1 to ${MAX_PAGE_SIZE}`);
      return n;
    },
  };
}
