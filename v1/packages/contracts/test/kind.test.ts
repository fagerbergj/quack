import assert from 'node:assert/strict';
import { test } from 'node:test';
import { breakingChanges, guideAnchors, restKind, validateSpec } from '../src/index.ts';
import { scheduleDocument, scheduleSpecV1, scheduleSpecV2, SLEEPER_PAGE } from './fixtures.ts';

const refs = { versioning: 'Keep the existing unversioned route, or add /v1?', time: 'How are run times written?' };
const mutated = (change: (doc: any) => void) => {
  const doc = structuredClone(scheduleDocument());
  change(doc);
  return doc;
};
const route = (doc: any) => doc.paths['/apps/sleeper/schedule'].get;

test('extract reads an OpenAPI file in YAML or JSON', () => {
  assert.deepEqual(restKind.extract('openapi: 3.1.0\npaths: {}\n'), { openapi: '3.1.0', paths: {} });
  assert.deepEqual(restKind.extract('{"openapi":"3.1.0"}'), { openapi: '3.1.0' });
});

test('check: the schedule document is clean, and each adjudicated rule catches its violation', async () => {
  assert.deepEqual(await restKind.check(scheduleDocument()), []);
  const cases: Array<[string, (doc: any) => void]> = [
    ['quack-openapi-31', (d) => { d.openapi = '3.0.3'; }],
    ['quack-self-contained', (d) => { route(d).responses[502].content['application/problem+json'].schema.$ref = 'https://example.org/p.json'; }],
    ['quack-no-get-body', (d) => { route(d).requestBody = { content: { 'application/json': { schema: { type: 'object' } } } }; }],
    ['quack-registered-status', (d) => { route(d).responses[299] = { description: 'made up' }; }],
    ['quack-object-top-level', (d) => { route(d).responses[200].content['application/json'].schema = { type: 'array', items: { type: 'string' } }; }],
    ['quack-problem-errors', (d) => { route(d).responses[502] = { description: 'plain', content: { 'application/json': { schema: { type: 'object' } } } }; }],
    ['quack-auth-recorded', (d) => { delete d.security; }],
    ['quack-deprecation-text', (d) => { route(d).deprecated = true; delete route(d).description; }],
  ];
  for (const [code, change] of cases) {
    const found = await restKind.check(mutated(change));
    assert.ok(found.some((f) => f.code === code), `${code} found: ${found.map((f) => f.code).join(', ')}`);
  }
});

test('breakingChanges (oasdiff): a type change breaks, an added optional field does not, a removed optional field does', async () => {
  const types = await breakingChanges(scheduleDocument('iso'), scheduleDocument('epoch'));
  assert.deepEqual(types.map((b) => [b.id, b.operation, b.path]), [['response-property-type-changed', 'GET', '/apps/sleeper/schedule']]);
  const added = mutated((d) => { route(d).responses[200].content['application/json'].schema.properties.runs.items.properties.note = { type: 'string' }; });
  assert.deepEqual(await breakingChanges(scheduleDocument(), added), []);
  assert.deepEqual((await breakingChanges(added, scheduleDocument())).map((b) => b.id), ['response-optional-property-removed']);
});

test('the guide has an anchor for every rule the spec cites', () => {
  const anchors = guideAnchors(restKind.skill);
  for (const a of ['versioning', 'errors', 'compatibility', 'paging', 'idempotency']) assert.ok(anchors.has(a), a);
});

test('validateSpec lists every structure problem, and wants changes and named callers from version 2', () => {
  assert.deepEqual(validateSpec(JSON.stringify(scheduleSpecV1(refs))), []);
  assert.deepEqual(validateSpec(JSON.stringify(scheduleSpecV2(refs, [SLEEPER_PAGE]))), []);
  assert.match(validateSpec('nope')[0], /not valid JSON/);
  const bare = { ...scheduleSpecV1(refs), choices: [{ id: 'x', pointer: '', decision: 'd', why: [] }] };
  assert.match(validateSpec(JSON.stringify(bare)).join('\n'), /choices\[0\] \(x\) has no why/);
  const noChanges = { ...scheduleSpecV1(refs), version: 2 };
  assert.deepEqual(validateSpec(JSON.stringify(noChanges)), ['version 2 must list its changes', 'compatibility needs breaking (what and callers) and safe']);
  const anon = { ...scheduleSpecV2(refs, []), compatibility: { breaking: [{ what: 'x', callers: [] }], safe: [] } };
  assert.deepEqual(validateSpec(JSON.stringify(anon)), ['each breaking entry needs what and at least one named caller']);
});
