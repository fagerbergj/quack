import { act, cleanup } from '@testing-library/react';
import { afterEach, beforeEach, expect, vi } from 'vitest';
import { forgetVersion } from './api.ts';

/** A route that answers with an error status instead of a JSON body. */
export class Fail {
  status: number;
  body: object;
  constructor(status: number, body: object = { title: 'Not Found', detail: 'not found' }) {
    this.status = status;
    this.body = body;
  }
}
export const notFound = new Fail(404);

export type Routes = Record<string, unknown>;
// The index and the build stamp are off unless a test turns them on; their 404 is how the app learns that.
const OFF: Routes = {
  'GET /index/approvals?state=pending': notFound, 'GET /index/questions?state=pending': notFound,
  'GET /index/failures': notFound, 'GET /index/artifacts': notFound, 'GET /version': notFound,
};
// A session's own reads answer empty, and an extension's service is not connected, unless a test routes them.
const QUIET = /^GET \/sessions\/[^/?]+\/(meta|questions|approvals)$/;
const APP = /^GET \/apps\//;

/** The answers for this test; a test adds or deletes entries in place. */
export const routes: Routes = {};
const unexpected: string[] = [];
/** What each session's socket reports on connect, by its key as the URL encodes it. */
export const live: Record<string, { artifacts?: object[]; approvals?: object[] }> = {};

const reply = (hit: unknown) => {
  const fail = hit instanceof Fail ? hit : undefined;
  return new Response(JSON.stringify(fail ? fail.body : hit), { status: fail?.status ?? 200, headers: { 'content-type': fail ? 'application/problem+json' : 'application/json' } });
};

// A request no test routed is a failure, not a silent 404: an extra fetch is a defect the test should see.
export const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const route = `${init?.method ?? 'GET'} /${url}`;
  const hit = routes[route] ?? routes[route.replace(/^GET \/sessions\?.*/, 'GET /sessions')] ?? OFF[route];
  if (hit !== undefined) return reply(hit);
  if (QUIET.test(route)) return reply({ [route.split('/').at(-1)!]: [] });
  if (APP.test(route)) return reply(notFound);
  unexpected.push(route);
  return reply(notFound);
});

export class FakeSocket {
  static all: FakeSocket[] = [];
  static OPEN = 1;
  sent: unknown[] = [];
  readyState = 1;
  onopen?: () => void;
  onclose?: () => void;
  onmessage?: (e: { data: string }) => void;
  url: string;
  constructor(url: string) {
    this.url = url;
    FakeSocket.all.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {}
  // Like the session, an open socket first reports the artifacts and what waits on a person.
  open() {
    act(() => this.onopen?.());
    const held = live[/\/sessions\/([^/]+)\/ws/.exec(this.url)?.[1] ?? ''];
    if (held?.artifacts) this.frame({ type: 'artifacts', artifacts: held.artifacts });
    if (held?.approvals) this.frame({ type: 'standing', questions: [], failures: [], approvals: held.approvals });
  }
  frame(f: object) {
    act(() => this.onmessage?.({ data: JSON.stringify(f) }));
  }
}

/** Stubs fetch, WebSocket and the viewport for each test, and fails a test that made a request it did not route. */
export function useHarness(base: () => Routes) {
  beforeEach(() => {
    FakeSocket.all = [];
    for (const k of Object.keys(routes)) delete routes[k];
    Object.assign(routes, base());
    for (const k of Object.keys(live)) delete live[k];
    unexpected.length = 0;
    fetchMock.mockClear();
    forgetVersion();
    vi.stubGlobal('WebSocket', FakeSocket);
    vi.stubGlobal('fetch', fetchMock);
    vi.stubGlobal('crypto', { randomUUID: () => 'uuid-1' });
    stubViewport(true);
    location.hash = '';
  });
  afterEach(() => {
    // Unmount first, so a failure below cannot leave this test's page in the next one.
    cleanup();
    vi.unstubAllGlobals();
    expect(unexpected.join('\n'), 'requests no route answered').toBe('');
  });
}

// Desktop puts the session list in the sidebar beside the main column; a phone shows one at a time.
export const stubViewport = (desktop: boolean, wide = false) =>
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('1440') ? wide : desktop, addEventListener() {}, removeEventListener() {} }));

export const usage = {};
export const entry = (id: string, model: object[]) => ({ id, conversationId: 'c', kind: 'x', model });
export const item = (e: object) => ({ kind: 'entry', entry: e });
// The conversation view as the socket sends it; omitted `entries` leave the transcript the client holds as it is.
export const view = (o: { entries?: object[]; run?: boolean | object; message?: object; tools?: object[]; usage?: object } = {}) => ({
  type: 'view',
  items: o.entries?.map(item),
  live: { ...(o.run ? { run: { taskId: 1, inputs: [], turns: 0 } } : {}), ...(o.message ? { generation: { attempt: 1, message: o.message } } : {}), ...(o.tools ? { tools: o.tools } : {}) },
  usage: o.usage ?? usage,
});
export const snapshot = (o: Parameters<typeof view>[0] = {}) => view({ entries: [], ...o });
export const assistant = (content: object[]) => ({ role: 'assistant', content, api: 'a', provider: 'p', model: 'm', usage, stopReason: 'stop', timestamp: 1 });
export const sess = (key: string, over: object = {}) => ({ key, agent: 'reviewer', running: false, active: false, createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T01:00:00.000Z', ...over });
