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

test('frames reassembles messages split across chunks', () => {
  const seen: Array<string | undefined> = [];
  const push = frames((_raw, m) => seen.push(m.method));
  const both = Buffer.concat([frame({ method: 'a' }), frame({ method: 'b' })]);
  push(both.subarray(0, 10));
  push(both.subarray(10, 40));
  push(both.subarray(40));
  assert.deepEqual(seen, ['a', 'b']);
});
