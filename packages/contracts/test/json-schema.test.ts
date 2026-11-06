import assert from 'node:assert/strict';
import { test } from 'node:test';
import { guideAnchors, jsonSchemaKind, kinds, lintJsonSchema, schemaBreaks, validateSpec } from '../src/index.ts';
import { settingsSchema, settingsSpec } from './fixtures.ts';

const mutated = (change: (s: any) => void) => {
  const s: any = structuredClone(settingsSchema());
  change(s);
  return s;
};
const codes = async (s: unknown) => (await lintJsonSchema(s)).map((f) => f.code);

test('the kind is registered and extracts JSON or YAML', () => {
  assert.equal(kinds['json-schema'], jsonSchemaKind);
  assert.deepEqual(jsonSchemaKind.extract('type: object\nproperties: {}\n'), { type: 'object', properties: {} });
  assert.deepEqual(jsonSchemaKind.extract('{"type":"string"}'), { type: 'string' });
  for (const a of ['extensibility', 'nullable', 'enums', 'compatibility']) assert.ok(guideAnchors(jsonSchemaKind.skill).has(a), a);
});

test('a described, closed 2020-12 schema has no findings', async () => {
  assert.deepEqual(await lintJsonSchema(settingsSchema()), []);
});

test('check names each mechanical problem with its path', async () => {
  const bad = mutated((s) => {
    delete s.$schema;
    delete s.properties.mode.description;
    delete s.additionalProperties;
    s.required.push('ghost');
    s.properties.nested = { type: 'object', description: 'x', properties: { a: { type: 'string', description: 'a' } } };
  });
  assert.deepEqual((await lintJsonSchema(bad)).map((f) => `${f.code} ${f.path}`).sort(), [
    'additional-properties-decided /',
    'additional-properties-decided /properties/nested',
    'property-description /properties/mode',
    'required-exists /required',
    'schema-declared /$schema',
  ]);
});

test('check reports an invalid or non-compiling schema', async () => {
  assert.deepEqual(await codes(mutated((s) => { s.properties.leagueId.type = 'strng'; })), ['schema-invalid']);
  assert.deepEqual(await codes(mutated((s) => { s.properties.leagueId.$ref = '#/$defs/missing'; })), ['schema-invalid']);
  assert.deepEqual(await codes('nope'), ['schema-invalid']);
});

test('diff reports removal, new required, narrowing and closing, each with a reason', async () => {
  const change = (s: any) => {
    delete s.properties.notifyTime;
    s.required.push('mode');
    s.properties.mode.enum = ['LINEUP'];
    s.properties.leagueId.type = 'integer';
  };
  const found = await schemaBreaks(mutated((s) => { s.additionalProperties = true; }), mutated(change));
  assert.deepEqual(found.map((b) => b.id).sort(), ['additional-properties-closed', 'enum-narrowed', 'property-removed', 'property-required', 'type-narrowed']);
  assert.ok(found.every((b) => b.text.length > 10));
  assert.equal(found.find((b) => b.id === 'property-removed')!.path, '/properties/notifyTime');
  assert.equal((await schemaBreaks(settingsSchema(), mutated(change))).length, 4, 'a schema that was already closed is not closed again');
});

test('diff treats additions and widening as safe', async () => {
  const next = mutated((s) => {
    s.properties.extra = { type: 'string', description: 'new optional' };
    s.properties.mode.enum.push('ALERTS');
    s.properties.leagueId.type = ['string', 'null'];
  });
  assert.deepEqual(await schemaBreaks(settingsSchema(), next), []);
  assert.deepEqual(await schemaBreaks({ type: 'integer' }, { type: 'number' }), []);
  assert.deepEqual((await schemaBreaks({ type: 'number' }, { type: 'integer' })).map((b) => b.id), ['type-narrowed']);
});

test('diff reports a bound that appears or tightens, and a loosened one is safe', async () => {
  const was = { type: 'string', minLength: 2, maxLength: 10 };
  const found = await schemaBreaks(was, { type: 'string', minLength: 3, maxLength: 5, pattern: '^a' });
  assert.deepEqual(found.map((b) => `${b.id} ${b.path}`).sort(), ['constraint-tightened /maxLength', 'constraint-tightened /minLength', 'constraint-tightened /pattern']);
  assert.deepEqual(await schemaBreaks(was, { type: 'string', minLength: 1, maxLength: 20 }), []);
});

test('a json-schema spec passes the structure check, and an unknown kind does not', () => {
  assert.deepEqual(validateSpec(JSON.stringify(settingsSpec())), []);
  assert.match(validateSpec(JSON.stringify({ ...settingsSpec(), kind: 'grpc' })).join(), /kind must be/);
});
