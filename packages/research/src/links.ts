import type { CodeResult } from '@quack/judge';
import type { Root } from 'mdast';
import { nodes, parse } from './markdown.ts';

/** What the session retrieved by the time the report was sealed: each fetch's source id, final URL and the URL asked for, and every search hit's URL. */
export type FetchLog = { fetched: Array<{ id: string; url: string; asked?: string }>; found: string[] };

// Query keys that track a visit or a session and never pick the page.
const TRACKING = /^(utm_\w+|fbclid|gclid|gclsrc|dclid|msclkid|yclid|igshid|mc_cid|mc_eid|_ga|_gl|_hsenc|_hsmi|ref|ref_src|ref_url|spm|amp|sessionid|session_id|sid|phpsessid|jsessionid)$/i;

const decoded = (path: string) => {
  try {
    return decodeURI(path);
  } catch {
    return path;
  }
};

/** One key per page: scheme, www/m/amp host prefix, fragment, tracking keys, key order, a trailing slash, index.html and an /amp suffix are dropped;
 *  the path and every other query key still tell pages apart. Undefined for anything but an http(s) URL. */
export function pageKey(raw: string): string | undefined {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return undefined;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return undefined;
  const host = u.hostname.replace(/^(www|m|mobile|amp)\./, '') + (u.port ? `:${u.port}` : '');
  const path = decoded(u.pathname.replace(/;jsessionid=[^/]*/i, '')).replace(/\/(index\.html?|amp)?\/?$/i, '');
  const query = [...u.searchParams].filter(([k]) => !TRACKING.test(k)).sort(([a, x], [b, y]) => a.localeCompare(b) || x.localeCompare(y));
  return `${host}${path}${query.length ? `?${new URLSearchParams(query)}` : ''}`;
}

/** The http(s) links of a report in order, each once: inline, reference definitions and bare URLs; code is quoted material, not a link. */
export const linksOf = (report: string) => refsOf(parse(report)).links;

// Marks are read off the same tree as links, so one in any code, an indented block included, cites nothing.
function refsOf(tree: Root) {
  const all = [...nodes(tree)];
  const urls = all.flatMap((n) => (n.type === 'link' || n.type === 'definition' ? [n.url] : []));
  const ids = all.flatMap((n) => (n.type === 'text' ? [...n.value.matchAll(/\[(s\d+)\]/g)].map((m) => m[1]) : []));
  return { links: [...new Set(urls.filter((u) => /^https?:/i.test(u)))], ids: [...new Set(ids)] };
}

const shared = (a: string, b: string) => {
  let i = 0;
  while (i < a.length && a[i] === b[i]) i++;
  return i;
};

/** The retrieved URL on the same host sharing the longest start with the link's key, so the writer sees which page it nearly was. */
function nearest(key: string, known: Map<string, string>): string | undefined {
  const host = key.split(/[/?]/, 1)[0];
  const same = [...known].filter(([k]) => k.split(/[/?]/, 1)[0] === host);
  return same.sort(([a], [b]) => shared(b, key) - shared(a, key))[0]?.[1];
}

function retrieved(log: FetchLog): Map<string, string> {
  const known = new Map<string, string>();
  for (const f of log.fetched) for (const u of [f.url, f.asked ?? f.url]) known.set(pageKey(u) ?? u, f.url);
  for (const u of log.found) if (!known.has(pageKey(u) ?? u)) known.set(pageKey(u) ?? u, u);
  return known;
}

/** Every link names a page this session fetched or saw in a search result, and every [sN] a fetch it made. */
export function linksRetrieved(report: string, log: FetchLog): CodeResult {
  const known = retrieved(log);
  const returned = new Set(log.fetched.map((f) => f.id));
  const { links, ids: cited } = refsOf(parse(report));
  const bad = links.filter((u) => !known.has(pageKey(u) ?? u));
  const unknown = cited.filter((id) => !returned.has(id));
  const notes = [
    ...bad.map((u) => {
      const near = nearest(pageKey(u) ?? u, known);
      return `${u} was neither fetched nor in a search result${near ? ` (the nearest page this session retrieved is ${near})` : ''}`;
    }),
    ...unknown.map((id) => `[${id}] is no source id a web_fetch returned`),
  ];
  const total = links.length + cited.length;
  if (!notes.length) return { met: 1 };
  return { met: (total - notes.length) / total, detail: `${notes.join('; ')}. Fetch each page you link with web_fetch, or drop the link.` };
}
