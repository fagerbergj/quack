import { lookup as dnsLookup, type LookupAddress } from 'node:dns';
import { request as httpRequest, type IncomingMessage } from 'node:http';
import { request as httpsRequest } from 'node:https';
import { BlockList, isIP } from 'node:net';

const NON_PUBLIC_V4 = new BlockList();
// Separate lists: a list holding ::ffff:0:0/96 also matches every plain IPv4 address.
const NON_PUBLIC_V6 = new BlockList();
const V4: Array<[string, number]> = [['0.0.0.0', 8], ['10.0.0.0', 8], ['100.64.0.0', 10], ['127.0.0.0', 8], ['169.254.0.0', 16], ['172.16.0.0', 12], ['192.0.0.0', 24], ['192.168.0.0', 16], ['198.18.0.0', 15], ['224.0.0.0', 3]];
// ::/96 covers unspecified, loopback and IPv4-compatible forms; NAT64 is refused whole since it wraps an IPv4 address.
const V6: Array<[string, number]> = [['::', 96], ['100::', 64], ['2001:db8::', 32], ['fc00::', 7], ['fe80::', 10], ['ff00::', 8], ['64:ff9b::', 96]];
for (const [a, p] of V4) NON_PUBLIC_V4.addSubnet(a, p, 'ipv4');
for (const [a, p] of V6) NON_PUBLIC_V6.addSubnet(a, p, 'ipv6');

// An IPv4-mapped address is judged by the IPv4 address it wraps.
function mappedV4(address: string): string | undefined {
  const m = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(new URL(`http://[${address}]`).hostname.slice(1, -1));
  if (!m) return undefined;
  const [hi, lo] = [parseInt(m[1], 16), parseInt(m[2], 16)];
  return `${hi >> 8}.${hi & 255}.${lo >> 8}.${lo & 255}`;
}

export const isPublicAddress = (address: string): boolean => {
  const family = isIP(address);
  if (family === 0) return false;
  if (family === 4) return !NON_PUBLIC_V4.check(address, 'ipv4');
  const v4 = mappedV4(address);
  return v4 ? isPublicAddress(v4) : !NON_PUBLIC_V6.check(address, 'ipv6');
};

export type GetOptions = { maxBytes: number; timeoutMs: number; /** Only tests replace the address policy; configuration never reaches it. */ isPublic?: (ip: string) => boolean; maxRedirects?: number };
export type Fetched = { url: string; status: number; contentType: string; body: Buffer };

// Validates at connect time, on the addresses the socket will use, so a name that re-resolves cannot rebind to a private host.
const publicLookup = (isPublic: (ip: string) => boolean) => (host: string, options: { all?: boolean; family?: number }, cb: (...args: any[]) => void) => {
  dnsLookup(host, { ...options, all: true }, (err, addrs: LookupAddress[]) => {
    if (err) return cb(err);
    const bad = addrs.find((a) => !isPublic(a.address));
    if (bad) return cb(new Error(`${host} resolves to a non-public address (${bad.address})`));
    return options.all ? cb(null, addrs) : cb(null, addrs[0].address, addrs[0].family);
  });
};

function read(res: IncomingMessage, maxBytes: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    res.on('data', (c: Buffer) => {
      size += c.length;
      if (size > maxBytes) return res.destroy(new Error(`response is larger than ${maxBytes} bytes`));
      chunks.push(c);
    });
    res.on('end', () => resolve(Buffer.concat(chunks)));
    res.on('error', reject);
  });
}

function once(url: URL, o: GetOptions): Promise<{ res: IncomingMessage; body: Buffer }> {
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return Promise.reject(new Error(`only http and https URLs are fetched, not ${url.protocol}`));
  const host = url.hostname.replace(/^\[|\]$/g, '');
  const isPublic = o.isPublic ?? isPublicAddress;
  if (isIP(host) && !isPublic(host)) return Promise.reject(new Error(`${host} is not a public address`));
  const send = url.protocol === 'https:' ? httpsRequest : httpRequest;
  return new Promise((resolve, reject) => {
    const req = send(url, { lookup: publicLookup(isPublic) as never, signal: AbortSignal.timeout(o.timeoutMs), headers: { 'user-agent': 'quack-research/1', accept: 'text/html,application/pdf,text/*;q=0.8' } }, (res) => {
      if (Number(res.headers['content-length']) > o.maxBytes) return res.destroy(new Error(`response is larger than ${o.maxBytes} bytes`));
      read(res, o.maxBytes).then((body) => resolve({ res, body }), reject);
    });
    req.on('error', reject);
    req.end();
  });
}

/** GET with redirects followed by hand, so every hop is checked; refuses non-public hosts and oversized bodies. */
export async function safeGet(start: string, o: GetOptions): Promise<Fetched> {
  let url = new URL(start);
  for (let hop = 0; hop <= (o.maxRedirects ?? 5); hop++) {
    const { res, body } = await once(url, o);
    const status = res.statusCode ?? 0;
    const location = res.headers.location;
    if (status >= 300 && status < 400 && location) {
      url = new URL(location, url);
      continue;
    }
    return { url: url.href, status, contentType: String(res.headers['content-type'] ?? ''), body };
  }
  throw new Error('too many redirects');
}
