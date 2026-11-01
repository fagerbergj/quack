import { createHash } from 'node:crypto';
import type { Context } from '@earendil-works/chord/context';
import { Type } from '@earendil-works/pi-ai';
import { defineDoc, defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import type { ClassifierRef, ModelRef } from '@quack/judge';
import { extractText, getDocumentProxy } from 'unpdf';
import { safeGet } from './net.ts';

export type ResearchConfig = {
  /** Exa's API origin; tests point it at a fake server. */
  exaUrl?: string;
  maxFetchBytes?: number;
  maxRevisions?: number;
  classifier?: ClassifierRef;
  judgeModel?: ModelRef;
};

export type SourceRecord = { id: string; url: string; fetchedAt: number; hash: string };

/** Everything the session fetched, in order; reports cite these ids and the judge checks the citations against them. */
export const SourcesDoc = defineDoc<{ sources: SourceRecord[] }>({
  kind: 'research.sources', version: 1, scope: 'conversation', history: 'latest', fork: 'current', initial: () => ({ sources: [] }), checkpointWhen: () => false,
});

const MAX_CHARS = 30_000;
const MAX_STORED_CHARS = 500_000;
const sourcePath = (hash: string) => `sources/${hash}.txt`;

/** The text kept for a fetched source, looked up by its id and keyed by the recorded hash; best-effort per session directory, so undefined when none was kept (a failed write, a fork with a fresh directory). */
export async function sourceText(api: ToolExecutionApi, id: string, ctx: Context): Promise<string | undefined> {
  const rec = (await api.snapshot(SourcesDoc, api.conversationId, ctx))?.sources.find((s) => s.id === id);
  const read = rec && (await api.env?.readTextFile(sourcePath(rec.hash), ctx));
  return read?.ok ? read.value : undefined;
}

const error = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true });

const ENTITIES: Record<string, string> = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ' };
const decode = (s: string) => s.replace(/&(#x?[0-9a-f]+|\w+);/gi, (m, e: string) => {
  if (e[0] !== '#') return ENTITIES[e.toLowerCase()] ?? m;
  const code = e[1].toLowerCase() === 'x' ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10);
  return code > 0 && code < 0x110000 ? String.fromCodePoint(code) : m;
});

// shortcut: regex stripping, no readability pass or script-rendered content; upgrade if pages come back as nav boilerplate.
export function htmlToText(html: string): string {
  const title = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(html)?.[1];
  const body = html
    .replace(/<(script|style|noscript|svg|head|title)\b[\s\S]*?<\/\1>/gi, '')
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/<\/(p|div|li|h[1-6]|tr|section|article)>|<br\s*\/?>/gi, '\n')
    .replace(/<[^>]+>/g, '');
  const text = decode(body).replace(/[ \t\f\r]+/g, ' ').replace(/ ?\n ?/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
  return title ? `${decode(title).trim()}\n\n${text}` : text;
}

async function pdfToText(body: Buffer): Promise<string> {
  const pdf = await getDocumentProxy(new Uint8Array(body));
  return (await extractText(pdf, { mergePages: true })).text;
}

async function toText(body: Buffer, contentType: string): Promise<string> {
  if (/pdf/i.test(contentType) || body.subarray(0, 5).toString() === '%PDF-') return pdfToText(body);
  if (/html/i.test(contentType)) return htmlToText(body.toString('utf8'));
  if (/^text\/|json|xml/i.test(contentType)) return body.toString('utf8');
  throw new Error(`cannot read ${contentType || 'an unknown content type'}; only HTML, PDF and text are supported`);
}

const MAX_BATCH = 5;
const CONCURRENCY = 3;
const BATCH_CHARS = 90_000;
const batch = (what: string) => Type.Array(Type.String(), { minItems: 1, maxItems: MAX_BATCH, description: what });

/** Results in input order, at most CONCURRENCY running at once; fn must catch its own failures so one item cannot fail the batch. */
async function mapLimit<T, R>(items: T[], fn: (item: T) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(items.length);
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(CONCURRENCY, items.length) }, async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]);
    }
  }));
  return out;
}

type Page = { text: string; record: Omit<SourceRecord, 'id'> } | { failed: string };

export function webFetchTool(config: ResearchConfig, isPublic?: (ip: string) => boolean) {
  const load = async (url: string): Promise<Page> => {
    try {
      const got = await safeGet(url, { maxBytes: config.maxFetchBytes ?? 5_000_000, timeoutMs: 20_000, isPublic });
      if (got.status >= 400) return { failed: `${got.url} answered ${got.status}` };
      return { text: await toText(got.body, got.contentType), record: { url: got.url, fetchedAt: 0, hash: createHash('sha256').update(got.body).digest('hex') } };
    } catch (err) {
      return { failed: `cannot fetch ${url}: ${(err as Error).message}` };
    }
  };
  return defineTool({
    name: 'web_fetch',
    description: `Fetch up to ${MAX_BATCH} public web pages or PDFs at once and return each one's text. A fetched page starts with a source id such as [s1]; cite that id in the report. A failed URL is reported on its own and does not fail the others. Private and local addresses are refused.`,
    parameters: Type.Object({ urls: batch('http(s) URLs') }),
    async execute(args, api, ctx) {
      // Network in parallel, bookkeeping one by one in input order so source ids do not depend on which page answers first.
      const pages = await mapLimit(args.urls, load);
      const cap = Math.min(MAX_CHARS, Math.floor(BATCH_CHARS / args.urls.length));
      const parts: string[] = [];
      for (const page of pages) {
        if ('failed' in page) {
          parts.push(page.failed);
          continue;
        }
        const record = { ...page.record, fetchedAt: api.now() };
        // Kept for later claim-support checks; a failed write is ignored so it loses only that input, not the fetch.
        await api.env?.writeFile(sourcePath(record.hash), page.text.slice(0, MAX_STORED_CHARS), ctx);
        const id = await api.commit(async (tx) => {
          const doc = await tx.doc(SourcesDoc, api.conversationId);
          const next = `s${doc.sources.length + 1}`;
          doc.sources = [...doc.sources, { id: next, ...record }];
          return next;
        }, ctx);
        const cut = page.text.length > cap ? `\n… cut at ${cap} of ${page.text.length} characters; read the rest with read_source id "${id}" offset ${cap}` : '';
        parts.push(`[${id}] ${record.url}\n\n${page.text.slice(0, cap)}${cut}`);
      }
      const text = parts.join('\n\n---\n\n');
      return pages.every((p) => 'failed' in p) ? error(text) : { output: [{ type: 'text', text }] };
    },
  });
}

export function readSourceTool() {
  return defineTool({
    name: 'read_source',
    description: `Read a window of ${MAX_CHARS} characters from an already fetched source, by its [sN] id and a character offset. Use it to continue a page web_fetch cut off; it never refetches.`,
    parameters: Type.Object({ id: Type.String({ description: 'Source id such as s1' }), offset: Type.Integer({ minimum: 0, description: 'Character to start at' }) }),
    replay: 'safe',
    async execute(args, api, ctx) {
      const id = args.id.replace(/^\[|\]$/g, '');
      const text = await sourceText(api, id, ctx);
      if (text === undefined) return error(`no stored text for source ${id}; ids come from web_fetch results in this session`);
      const end = args.offset + MAX_CHARS;
      const body = text.slice(args.offset, end);
      const next = end < text.length ? `\n… ${end} of ${text.length} characters; continue with read_source id "${id}" offset ${end}` : `\n… end of source ${id} (${text.length} characters)`;
      return { output: [{ type: 'text', text: body + next }] };
    },
  });
}

type ExaResult = { title?: string; url: string; publishedDate?: string; text?: string };

export function formatResults(results: ExaResult[]): string {
  if (!results.length) return 'No results.';
  return results.map((r, i) => `${i + 1}. ${r.title || r.url}${r.publishedDate ? ` (${r.publishedDate.slice(0, 10)})` : ''}\n   ${r.url}${r.text ? `\n   ${r.text.replace(/\s+/g, ' ').trim()}` : ''}`).join('\n');
}

export function webSearchTool(config: ResearchConfig) {
  // Read once: the runtime drops declared variables from the process after load, so no child process inherits the key.
  const apiKey = process.env.QUACK_EXA_API_KEY;
  const search = async (query: string, count: number): Promise<string> => {
    // Body reads are inside the try: a malformed or dropped body fails this query, not the batch.
    try {
      const res = await fetch(`${config.exaUrl ?? 'https://api.exa.ai'}/search`, {
        method: 'POST',
        headers: { 'x-api-key': apiKey!, 'content-type': 'application/json' },
        body: JSON.stringify({ query, numResults: count, contents: { text: { maxCharacters: 400 } } }),
        signal: AbortSignal.timeout(20_000),
      });
      if (!res.ok) return `search failed: ${res.status} ${(await res.text()).slice(0, 200)}`;
      return formatResults(((await res.json()) as { results?: ExaResult[] }).results ?? []);
    } catch (err) {
      return `search failed: ${(err as Error).message}`;
    }
  };
  return defineTool({
    name: 'web_search',
    description: `Search the web (Exa) for up to ${MAX_BATCH} queries at once; each gets titles, URLs, dates and a snippet per hit, or its own failure. Fetch a hit with web_fetch before citing it.`,
    parameters: Type.Object({ queries: batch('Search queries'), count: Type.Optional(Type.Integer({ minimum: 1, maximum: 20, description: 'Hits per query' })) }),
    replay: 'safe',
    async execute(args, _api, _ctx) {
      if (!apiKey) return error('web_search is not configured: QUACK_EXA_API_KEY is not set');
      const results = await mapLimit(args.queries, (q) => search(q, args.count ?? 8));
      return { output: [{ type: 'text', text: results.map((r, i) => `## ${args.queries[i]}\n${r}`).join('\n\n') }] };
    },
  });
}
