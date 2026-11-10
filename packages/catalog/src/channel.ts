// The lifecycle channel's HTTP adapter: POST /sessions/{key}/events on the credentialed session listener.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendProblem, serviceHttp, tooLarge } from '@quack/sdk';
import { BatchError, type Catalog, type CatalogEvent, type CoreEvent } from './core.ts';
import { SESSION_KEY } from './http.ts';

const ROUTE = /^\/sessions\/([^/]+)\/events$/;
export const MAX_EVENTS = 500;
const EXTENSION = /^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$/;
const { send, json } = serviceHttp({ maxBody: 1024 * 1024 });

type Obj = Record<string, unknown>;
class Bad extends Error {}
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);
const need = (ok: boolean, what: string) => { if (!ok) throw new Bad(what); };
const str = (o: Obj, k: string, what: string, optional = false) => {
  if (o[k] === undefined && optional) return undefined;
  need(typeof o[k] === 'string', `${what}.${k} must be a string`);
  return o[k] as string;
};
const int = (o: Obj, k: string, what: string, min = 1) => (need(Number.isSafeInteger(o[k]) && (o[k] as number) >= min, `${what}.${k} must be an integer of at least ${min}`), o[k] as number);
const date = (o: Obj, k: string, what: string) => {
  const ms = Date.parse(str(o, k, what)!);
  need(!Number.isNaN(ms) && /^\d{4}-\d{2}-\d{2}T/.test(o[k] as string), `${what}.${k} must be an RFC 3339 date-time`);
  return ms;
};
const optStr = (o: Obj, k: string, what: string) => (o[k] === null ? undefined : str(o, k, what, true));
const strings = (v: unknown) => Array.isArray(v) && v.every((s) => typeof s === 'string');

function principal(v: unknown, what: string) {
  need(isObj(v) && (v.kind === 'tailnet' || (v.kind === 'person' && typeof v.id === 'string' && v.id !== '')), `${what}.by must be a person with an id or the tailnet`);
  return (v as Obj).kind === 'person' ? { kind: 'person' as const, id: (v as Obj).id as string } : { kind: 'tailnet' as const };
}

function money(v: unknown, what: string) {
  need(isObj(v) && typeof v.currencyCode === 'string' && /^[A-Z]{3}$/.test(v.currencyCode) && typeof v.amountMicros === 'string' && /^\d{1,19}$/.test(v.amountMicros), `${what}.cost must be Money`);
  const micros = BigInt((v as Obj).amountMicros as string);
  need(micros < 2n ** 63n, `${what}.cost.amountMicros must fit an int64`);
  return { currencyCode: (v as Obj).currencyCode as string, amountMicros: micros };
}

function tokens(v: unknown, what: string) {
  need(isObj(v), `${what}.tokens must be an object`);
  const t = v as Obj;
  return { input: int(t, 'input', `${what}.tokens`, 0), output: int(t, 'output', `${what}.tokens`, 0), cacheRead: int(t, 'cacheRead', `${what}.tokens`, 0), cacheWrite: int(t, 'cacheWrite', `${what}.tokens`, 0) };
}

function prompt(v: unknown, what: string) {
  need(isObj(v) && typeof v.question === 'string' && strings(v.options) && typeof v.freeText === 'boolean', `${what}.prompt must have a question, options and freeText`);
  const p = v as Obj;
  for (const k of ['evidence', 'header', 'title', 'detail']) need(p[k] === undefined || p[k] === null || typeof p[k] === 'string', `${what}.prompt.${k} must be a string or null`);
  need(p.suggested === undefined || p.suggested === null || Number.isSafeInteger(p.suggested), `${what}.prompt.suggested must be an integer or null`);
  need(p.optionDescriptions === undefined || (isObj(p.optionDescriptions) && Object.values(p.optionDescriptions).every((d) => typeof d === 'string')), `${what}.prompt.optionDescriptions must map labels to strings`);
  return p as never;
}

// A seal carries no deliveries (channel v9): they arrive only as artifact.delivered events.
function sealed(d: Obj, what: string) {
  return {
    kind: str(d, 'kind', what)!, version: int(d, 'version', what), hash: str(d, 'hash', what)!, content: str(d, 'content', what)!,
    ...(d.sealedAt !== undefined && { sealedAt: date(d, 'sealedAt', what) }), ...(optStr(d, 'summary', what) !== undefined && { summary: optStr(d, 'summary', what) }),
  };
}

function delivered(d: Obj, what: string) {
  for (const k of ['where', 'link']) need(typeof d[k] === 'string' && (d[k] as string).length > 0, `${what}.${k} must be a non-empty string`);
  return { kind: str(d, 'kind', what)!, version: int(d, 'version', what), where: d.where as string, link: d.link as string, at: date(d, 'at', what) };
}

const steps = (v: unknown, what: string) => (need(Array.isArray(v) && v.every((s) => isObj(s) && typeof s.step === 'string' && typeof s.error === 'string'), `${what}.failedSteps must be {step, error} pairs`), v as Array<{ step: string; error: string }>);

// Each event type's data, checked and turned into domain values.
const DATA: Record<CoreEvent['type'], (d: Obj, what: string) => unknown> = {
  'session.created': (d, w) => (need(d.version === undefined || d.version === null || Number.isSafeInteger(d.version), `${w}.version must be an integer or null`), { agent: str(d, 'agent', w), version: (d.version as number | null | undefined) ?? null, createdAt: date(d, 'createdAt', w) }),
  'run.started': (d, w) => ({ run: int(d, 'run', w) }),
  'run.ended': (d, w) => {
    need(['succeeded', 'denied', 'failed', 'aborted'].includes(d.outcome as string), `${w}.outcome must be succeeded, denied, failed or aborted`);
    return { run: int(d, 'run', w), outcome: d.outcome, failedSteps: steps(d.failedSteps, w), ...(d.lastMessage !== undefined && { lastMessage: str(d, 'lastMessage', w) }) };
  },
  'turn.ended': (d, w) => ({ run: int(d, 'run', w), index: int(d, 'index', w) }),
  'ask.opened': (d, w) => {
    need(d.type === 'approval' || d.type === 'question', `${w}.type must be approval or question`);
    need(d.type !== 'approval' || (isObj(d.toolCall) && typeof d.toolCall.tool === 'string' && 'args' in d.toolCall), `${w}.toolCall must have a tool and args`);
    return { id: str(d, 'id', w), type: d.type, createdAt: date(d, 'createdAt', w), ...(d.toolCall !== undefined && { toolCall: d.toolCall }), ...(d.prompt !== undefined && { prompt: prompt(d.prompt, w) }) };
  },
  'ask.resolved': (d, w) => ({ id: str(d, 'id', w), choice: str(d, 'choice', w), ...(d.text !== undefined && { text: str(d, 'text', w) }), ...(d.reason !== undefined && { reason: str(d, 'reason', w) }), by: principal(d.by, w) }),
  'ask.cancelled': (d, w) => ({ id: str(d, 'id', w), run: int(d, 'run', w) }),
  'artifact.sealed': sealed,
  'spend.recorded': (d, w) => ({ cost: money(d.cost, w), tokens: tokens(d.tokens, w) }),
  'title.set': (d, w) => (need(typeof d.title === 'string' && d.title.length <= 120, `${w}.title must be a string of at most 120 characters`), { title: d.title }),
  'state.changed': (d, w) => (need(['running', 'idle', 'stopped', 'archived'].includes(d.state as string), `${w}.state must be running, idle, stopped or archived`), { state: d.state }),
  'artifact.delivered': delivered,
};

/** A batch as the channel carries it, in domain values; throws Bad with what is malformed. */
export function parseBatch(body: unknown): CatalogEvent[] {
  need(isObj(body) && Array.isArray(body.events) && body.events.length >= 1, 'events must be a non-empty array');
  const events = (body as { events: unknown[] }).events;
  need(events.length <= MAX_EVENTS, `a batch carries at most ${MAX_EVENTS} events`);
  return parseEvents(events);
}

/** Channel events in seq order, as a batch or a session's whole recorded stream carries them. */
export function parseEvents(events: unknown[]): CatalogEvent[] {
  let last = 0;
  return events.map((e, i) => {
    const what = `events[${i}]`;
    const core = isObj(e) && Object.hasOwn(DATA, e.type as string);
    // shortcut: an extension event's data is not checked against its manifest's schema until SDK v2 manifests declare events.
    const extension = isObj(e) && !core && typeof e.type === 'string' && EXTENSION.test(e.type);
    need(isObj(e) && isObj(e.data) && (core || extension), `${what} must be one of the channel's event types, or a namespaced <extension>.<event>, with data`);
    const o = e as Obj;
    const seq = int(o, 'seq', what);
    need(seq > last, `${what}.seq must strictly increase within the batch`);
    last = seq;
    need((o.id as string)?.length > 0, `${what}.id must be a non-empty string`);
    const head = { seq, id: str(o, 'id', what)!, at: date(o, 'at', what), type: o.type as string };
    return (extension ? { ...head, data: o.data, extension: true } : { ...head, data: DATA[o.type as CoreEvent['type']](o.data as Obj, `${what}.data`) }) as CatalogEvent;
  });
}

const decode = (s: string) => { try { return decodeURIComponent(s); } catch { return undefined; } };

/** Handles the channel route for a caller the session listener authenticated; false for any other request. `onApplied`
 * hears each accepted batch, replays included, and never slows or fails the reply. */
export function eventsRoute(catalog: Catalog, onApplied?: (key: string, events: CatalogEvent[]) => Promise<unknown>) {
  return async (req: IncomingMessage, res: ServerResponse, caller: string): Promise<boolean> => {
    const match = req.method === 'POST' && ROUTE.exec(new URL(req.url ?? '', 'http://distributor').pathname);
    if (!match) return false;
    const key = decode(match[1]!);
    // The key is an object boundary the credential cannot reach, so a foreign one is not found.
    if (!key || !SESSION_KEY.test(key) || key !== caller) return sendProblem(res, 404, 'no such session for this credential'), true;
    let events: CatalogEvent[];
    try {
      events = parseBatch(await json(req));
    } catch (err) {
      if (tooLarge(err)) return sendProblem(res, 413, 'the batch is over the 1 MiB body limit; split it at a seq boundary'), true;
      return sendProblem(res, 400, err instanceof Bad || err instanceof SyntaxError ? err.message : 'malformed batch'), true;
    }
    try {
      const out = await catalog.publish(key, events);
      if (out.gap) res.writeHead(409, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ type: 'about:blank', title: 'Conflict', status: 409, detail: `the batch starts past seq ${out.acked + 1}`, error: `the batch starts past seq ${out.acked + 1}`, acked: out.acked }));
      else {
        send(res, 200, { acked: out.acked });
        void onApplied?.(key, events).catch((err) => console.error('catalog: after a batch', err));
      }
    } catch (err) {
      if (err instanceof BatchError) sendProblem(res, 400, err.message);
      else sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
    }
    return true;
  };
}

