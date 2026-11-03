import assert from 'node:assert/strict';
import { test } from 'node:test';
import { frames, holder } from '../src/lsp-hold.ts';

const frame = (m: object) => { const body = JSON.stringify(m); return Buffer.from(`Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`); };
const references = { method: 'textDocument/references' };

test('references wait for the load progress to end; other requests pass', () => {
  const sent: string[] = [];
  const h = holder((raw) => sent.push(JSON.parse(raw.subarray(raw.indexOf('\r\n\r\n') + 4).toString()).method));
  h.fromClient(frame(references), references);
  h.fromServer({ method: 'window/workDoneProgress/create', params: { token: 't' } });
  h.fromClient(frame({ method: 'textDocument/documentSymbol' }), { method: 'textDocument/documentSymbol' });
  assert.deepEqual(sent, ['textDocument/documentSymbol'], 'asked before the server announced its load');
  h.fromServer({ method: '$/progress', params: { token: 't', value: { kind: 'report' } } });
  assert.equal(sent.length, 1);
  h.fromServer({ method: '$/progress', params: { token: 't', value: { kind: 'end' } } });
  assert.deepEqual(sent, ['textDocument/documentSymbol', 'textDocument/references']);
  h.fromClient(frame(references), references);
  assert.equal(sent.length, 3, 'once loaded, references go straight through');
});

function clock() {
  const timers: Array<{ fn: () => void; live: boolean }> = [];
  const arm = (fn: () => void) => { const t = { fn, live: true }; timers.push(t); return () => { t.live = false; }; };
  return { arm, fire: () => timers.filter((t) => t.live).forEach((t) => { t.live = false; t.fn(); }), live: () => timers.filter((t) => t.live).length };
}
const methodOf = (raw: Buffer) => JSON.parse(raw.subarray(raw.indexOf('\r\n\r\n') + 4).toString()).method;

test('a server that reports no progress releases held references after the quiet period', () => {
  const sent: string[] = [];
  const c = clock();
  const h = holder((raw) => sent.push(methodOf(raw)), 5000, c.arm);
  h.fromClient(frame(references), references);
  assert.deepEqual(sent, []);
  c.fire();
  assert.deepEqual(sent, ['textDocument/references']);
  h.fromClient(frame(references), references);
  assert.equal(sent.length, 2, 'after release, references pass straight through');
});

test('progress restarts the quiet period; the end of the load releases and cancels the timer', () => {
  const sent: string[] = [];
  const c = clock();
  const h = holder((raw) => sent.push(methodOf(raw)), 5000, c.arm);
  h.fromClient(frame(references), references);
  h.fromServer({ method: 'window/workDoneProgress/create', params: { token: 't' } });
  assert.equal(c.live(), 1, 'one timer, restarted by the progress message');
  h.fromServer({ method: '$/progress', params: { token: 't', value: { kind: 'end' } } });
  assert.equal(sent.length, 1);
  assert.equal(c.live(), 0);
});

test('frames reassembles messages split across chunks', () => {
  const seen: Array<string | undefined> = [];
  const push = frames((_raw, m) => seen.push(m.method));
  const both = Buffer.concat([frame({ method: 'a' }), frame({ method: 'b' })]);
  push(both.subarray(0, 10));
  push(both.subarray(10, 40));
  push(both.subarray(40));
  assert.deepEqual(seen, ['a', 'b']);
});
