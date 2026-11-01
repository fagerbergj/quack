import { createHash } from 'node:crypto';
import { Type } from '@earendil-works/pi-ai';
import { defineDoc, defineTool } from '@earendil-works/pi-durable';
import type { ClassifierRef, ModelRef } from '@quack/judge';
import { extractText, getDocumentProxy } from 'unpdf';
import { safeGet } from './net.ts';

export type ResearchConfig = {
  /** Exa's API origin; tests point it at a fake server. */
  exaUrl?: string;
  /** Lets fetches reach private hosts; for tests against local fake servers only. */
  allowPrivate?: boolean;
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

export function webFetchTool(config: ResearchConfig) {
  return defineTool({
    name: 'web_fetch',
    description: `Fetch a public web page or PDF and return its text (first ${MAX_CHARS} characters). The result starts with a source id such as [s1]; cite that id in the report. Private and local addresses are refused.`,
    parameters: Type.Object({ url: Type.String({ description: 'An http(s) URL' }) }),
    async execute(args, api, ctx) {
      let text: string;
      let record: Omit<SourceRecord, 'id'>;
      try {
        const got = await safeGet(args.url, { maxBytes: config.maxFetchBytes ?? 5_000_000, timeoutMs: 20_000, allowPrivate: config.allowPrivate });
        if (got.status >= 400) return error(`${got.url} answered ${got.status}`);
        text = await toText(got.body, got.contentType);
        record = { url: got.url, fetchedAt: api.now(), hash: createHash('sha256').update(got.body).digest('hex') };
      } catch (err) {
        return error(`cannot fetch ${args.url}: ${(err as Error).message}`);
      }
      const id = await api.commit(async (tx) => {
        const doc = await tx.doc(SourcesDoc, api.conversationId);
        const next = `s${doc.sources.length + 1}`;
        doc.sources = [...doc.sources, { id: next, ...record }];
        return next;
      }, ctx);
      const cut = text.length > MAX_CHARS ? `\n… cut at ${MAX_CHARS} of ${text.length} characters` : '';
      return { output: [{ type: 'text', text: `[${id}] ${record.url}\n\n${text.slice(0, MAX_CHARS)}${cut}` }] };
    },
  });
}

type ExaResult = { title?: string; url: string; publishedDate?: string; text?: string };

export function formatResults(results: ExaResult[]): string {
  if (!results.length) return 'No results.';
  return results.map((r, i) => `${i + 1}. ${r.title || r.url}${r.publishedDate ? ` (${r.publishedDate.slice(0, 10)})` : ''}\n   ${r.url}${r.text ? `\n   ${r.text.replace(/\s+/g, ' ').trim()}` : ''}`).join('\n');
}

export function webSearchTool(config: ResearchConfig) {
  return defineTool({
    name: 'web_search',
    description: 'Search the web (Exa) and return titles, URLs, dates and a snippet for each hit. Fetch a hit with web_fetch before citing it.',
    parameters: Type.Object({ query: Type.String(), count: Type.Optional(Type.Integer({ minimum: 1, maximum: 20 })) }),
    replay: 'safe',
    async execute(args, _api, _ctx) {
      const key = process.env.QUACK_EXA_API_KEY;
      if (!key) return error('web_search is not configured: QUACK_EXA_API_KEY is not set');
      const res = await fetch(`${config.exaUrl ?? 'https://api.exa.ai'}/search`, {
        method: 'POST',
        headers: { 'x-api-key': key, 'content-type': 'application/json' },
        body: JSON.stringify({ query: args.query, numResults: args.count ?? 8, contents: { text: { maxCharacters: 400 } } }),
        signal: AbortSignal.timeout(20_000),
      }).catch((err: Error) => err);
      if (res instanceof Error) return error(`search failed: ${res.message}`);
      if (!res.ok) return error(`search failed: ${res.status} ${(await res.text()).slice(0, 200)}`);
      return { output: [{ type: 'text', text: formatResults(((await res.json()) as { results?: ExaResult[] }).results ?? []) }] };
    },
  });
}
