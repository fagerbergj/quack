import { Ajv2020 } from 'ajv/dist/2020.js';
import type { Finding } from './rest-rules.ts';

export const DRAFT_2020_12 = 'https://json-schema.org/draft/2020-12/schema';

type Schema = Record<string, unknown>;
const isObj = (v: unknown): v is Schema => typeof v === 'object' && v !== null && !Array.isArray(v);
export const esc = (s: string) => s.replaceAll('~', '~0').replaceAll('/', '~1');

const CONDITIONAL = ['if', 'then', 'else'];

/** Every subschema with its JSON pointer and whether it sits under if/then/else; only the keywords this kind's rules and diff care about are followed. */
export function* subschemas(s: unknown, at = '', conditional = false): Generator<[Schema, string, boolean]> {
  if (!isObj(s)) return;
  yield [s, at, conditional];
  for (const k of ['properties', '$defs', 'patternProperties', 'dependentSchemas']) if (isObj(s[k])) for (const [n, c] of Object.entries(s[k])) yield* subschemas(c, `${at}/${k}/${esc(n)}`, conditional);
  for (const k of ['items', 'additionalProperties', 'unevaluatedProperties', 'contains', 'not', ...CONDITIONAL, 'propertyNames']) yield* subschemas(s[k], `${at}/${k}`, conditional || CONDITIONAL.includes(k));
  for (const k of ['allOf', 'anyOf', 'oneOf', 'prefixItems']) if (Array.isArray(s[k])) for (const [i, c] of s[k].entries()) yield* subschemas(c, `${at}/${k}/${i}`, conditional);
}

const text = (v: unknown) => typeof v === 'string' && v.trim() !== '';
const find = (code: string, message: string, path: string): Finding => ({ code, message, path: path || '/' });

function validity(doc: Schema): Finding[] {
  // Formats are annotations here; the contract fixes the shape, not which format checkers a consumer has.
  const ajv = new Ajv2020({ strict: false, validateFormats: false });
  if (!ajv.validateSchema(doc)) return [find('schema-invalid', `not a valid JSON Schema 2020-12 document: ${ajv.errorsText(ajv.errors)}`, '')];
  try {
    ajv.compile(doc as Schema);
    return [];
  } catch (err) {
    return [find('schema-invalid', `does not compile: ${(err as Error).message}`, '')];
  }
}

function objectProblems(s: Schema, at: string): Finding[] {
  const out: Finding[] = [];
  const props = isObj(s.properties) ? s.properties : undefined;
  if ((props || s.type === 'object') && s.additionalProperties === undefined && s.unevaluatedProperties === undefined) out.push(find('additional-properties-decided', 'an object schema must say whether unknown properties are allowed (additionalProperties true or false)', at));
  if (props && Array.isArray(s.required)) for (const r of s.required) if (!Object.hasOwn(props, String(r))) out.push(find('required-exists', `required lists "${String(r)}", which is not in properties`, `${at}/required`));
  if (props) for (const [n, p] of Object.entries(props)) if (!isObj(p) || !text(p.description)) out.push(find('property-description', `property "${n}" has no description`, `${at}/properties/${esc(n)}`));
  return out;
}

/** Mechanical problems in a JSON Schema 2020-12 document. */
export async function lintJsonSchema(document: unknown): Promise<Finding[]> {
  if (!isObj(document)) return [find('schema-invalid', 'the document must be a JSON Schema object', '')];
  const out = validity(document);
  if (document.$schema !== DRAFT_2020_12) out.push(find('schema-declared', `$schema must be "${DRAFT_2020_12}"`, '/$schema'));
  // A conditional branch constrains an object its parent defines (and closing it there would reject the parent's own
  // properties), so the object rules hold for the parent only.
  for (const [s, at, conditional] of subschemas(document)) if (!conditional) out.push(...objectProblems(s, at));
  return out;
}
