import ts from 'typescript';

/** One top-level definition of a contract document; `id` is its JSON pointer, or METHOD path for an operation. Text for typescript. */
export type Definition = { id: string; definition: unknown };

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);
const esc = (key: string) => key.replaceAll('~', '~0').replaceAll('/', '~1');
const METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];
// What is left once the definitions are taken out; it can carry guarantees of its own (a root description, top-level security).
const REST = '(rest of the document)';

function without(doc: Obj, keys: string[]): Obj {
  return Object.fromEntries(Object.entries(doc).filter(([k]) => !keys.includes(k)));
}

const tail = (rest: Obj): Definition[] => (Object.keys(rest).length ? [{ id: REST, definition: rest }] : []);

// Each operation carries its path item's shared fields (path parameters, summary), which apply to it.
function operations(items: unknown, at: (key: string) => string): Definition[] {
  if (!isObj(items)) return [];
  return Object.entries(items).flatMap(([key, item]) => {
    if (!isObj(item)) return [{ id: at(key), definition: item }];
    const shared = without(item, METHODS);
    const ops = METHODS.filter((m) => m in item);
    if (!ops.length) return [{ id: at(key), definition: item }];
    return ops.map((m) => ({ id: `${m.toUpperCase()} ${at(key)}`, definition: Object.keys(shared).length ? { ...shared, [m]: item[m] } : item[m] }));
  });
}

const entries = (doc: Obj, key: string, prefix: string): Definition[] =>
  isObj(doc[key]) ? Object.entries(doc[key]).map(([k, v]) => ({ id: `${prefix}/${esc(k)}`, definition: v })) : [];

/** OpenAPI: every operation (paths and webhooks), every named component, then the rest. */
export function openApiDefinitions(doc: unknown): Definition[] {
  if (!isObj(doc)) return [];
  const components = isObj(doc.components) ? doc.components : {};
  const sections = Object.keys(components).filter((k) => isObj(components[k]));
  const rest = without(doc, ['paths', 'webhooks', 'components']);
  const left = without(components, sections);
  if (Object.keys(left).length) rest.components = left;
  return [
    ...operations(doc.paths, (p) => p),
    ...operations(doc.webhooks, (w) => `webhook ${w}`),
    ...sections.flatMap((s) => entries(components, s, `/components/${esc(s)}`)),
    ...tail(rest),
  ];
}

const SCHEMA_MAPS = ['properties', 'patternProperties', '$defs', 'definitions'];

/** JSON Schema: every root property, pattern property and $defs entry, then the rest of the root. */
export function jsonSchemaDefinitions(doc: unknown): Definition[] {
  if (!isObj(doc)) return [];
  const maps = SCHEMA_MAPS.filter((k) => isObj(doc[k]));
  return [...maps.flatMap((k) => entries(doc, k, `/${k}`)), ...tail(without(doc, maps))];
}

function declaredName(s: ts.Statement): string | undefined {
  if (ts.isVariableStatement(s)) return s.declarationList.declarations.map((d) => d.name.getText()).join(', ');
  if (ts.isModuleDeclaration(s) || ts.isFunctionDeclaration(s) || ts.isClassDeclaration(s) || ts.isInterfaceDeclaration(s) || ts.isTypeAliasDeclaration(s) || ts.isEnumDeclaration(s)) return s.name?.getText();
  return undefined;
}

/** TypeScript: every top-level declaration with its TSDoc, merged and overloaded ones together, then the rest of the file (its header, imports, re-exports). */
export function typeScriptDefinitions(doc: unknown): Definition[] {
  if (typeof doc !== 'string') return [];
  const sf = ts.createSourceFile('contract.d.ts', doc, ts.ScriptTarget.ES2022, true);
  const byName = new Map<string, string[]>();
  let rest = '';
  let from = 0;
  for (const s of sf.statements) {
    const name = declaredName(s);
    if (!name) continue;
    const start = s.getStart(sf, true);
    rest += doc.slice(from, start);
    from = s.end;
    byName.set(name, [...(byName.get(name) ?? []), doc.slice(start, s.end)]);
  }
  rest = (rest + doc.slice(from)).trim();
  const out = [...byName].map(([name, parts]) => ({ id: `/${name}`, definition: parts.join('\n\n') }));
  return rest ? [...out, { id: REST, definition: rest }] : out;
}
