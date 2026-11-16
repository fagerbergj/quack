// No node: imports, so the web bundle imports the artifact's type and parser from here.

export type Why = { source: 'answer' | 'guide' | 'code'; text: string; /** A question id, a guide rule anchor, or path:line. */ ref: string };
export type Choice = { id: string; /** JSON pointer into `document` the choice governs; empty when spec-wide. */ pointer: string; decision: string; why: Why[] };
export type Change = { choice: string; was: string; now: string; why: string };
/** Required at seal since contract.spec v2; a stored entry may lack migration, say change for what, or name its callers in one string. */
export type Breaking = { what: string; callers: string[]; migration?: string };
export type Compatibility = { breaking: Breaking[]; safe: string[] };
export type Intent = { purpose: string; nonGoals: string[]; audience: string; consumers: string[]; guarantees: string[] };

export const SPEC_KINDS = ['rest', 'json-schema', 'typescript'] as const;

/** The sealed `contract.spec` artifact: one JSON file per spec version. */
export type ContractSpec = {
  kind: (typeof SPEC_KINDS)[number];
  title: string;
  version: number;
  intent: Intent;
  /** The kind's standard document; an OpenAPI 3.1 object for rest, a JSON Schema 2020-12 schema for json-schema, the .d.ts source text for typescript. */
  document: unknown;
  choices: Choice[];
  /** What changes and compatibility are measured against: the last approved version's number, or 'shipped'; absent on a greenfield surface. */
  baseline?: number | 'shipped';
  changes?: Change[];
  compatibility?: Compatibility;
};

export const CONTRACT_KIND = 'contract.spec';
export const CONTRACT_PATH = 'artifacts/spec.json';

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const strings = (v: unknown) => Array.isArray(v) && v.every((s) => typeof s === 'string');
const text = (v: unknown) => typeof v === 'string' && v.trim() !== '';

export function parseSpec(content: string): ContractSpec | string {
  try {
    const v = JSON.parse(content);
    return isObj(v) ? (v as ContractSpec) : 'the file must hold one JSON object';
  } catch (err) {
    return `the file is not valid JSON: ${(err as Error).message}`;
  }
}

function intentProblems(i: unknown): string[] {
  if (!isObj(i)) return ['intent is missing'];
  const bad = ['purpose', 'audience'].filter((k) => !text(i[k])).map((k) => `intent.${k} must be a non-empty string`);
  return bad.concat(['nonGoals', 'consumers', 'guarantees'].filter((k) => !strings(i[k])).map((k) => `intent.${k} must be a list of strings`));
}

function choiceProblems(c: unknown, i: number): string[] {
  const at = `choices[${i}]`;
  if (!isObj(c)) return [`${at} must be an object`];
  const out = ['id', 'decision'].filter((k) => !text(c[k])).map((k) => `${at}.${k} must be a non-empty string`);
  if (typeof c.pointer !== 'string') out.push(`${at}.pointer must be a string (empty when spec-wide)`);
  const why = Array.isArray(c.why) ? c.why : [];
  if (!why.length) out.push(`${at} (${String(c.id)}) has no why; cite an answer, a guide rule or a code path`);
  for (const w of why) if (!isObj(w) || !['answer', 'guide', 'code'].includes(String(w.source)) || !text(w.text) || !text(w.ref)) out.push(`${at}.why entries need source (answer|guide|code), text and ref`);
  return out;
}

const breakingOk = (b: unknown) => isObj(b) && text(b.what) && strings(b.callers) && (b.callers as string[]).length > 0 && text(b.migration);

// A revision of an approved version must change something; a spec of a shipped surface may write it down as it stands.
function baselinedProblems(s: Record<string, unknown>): string[] {
  const out: string[] = [];
  const changes = Array.isArray(s.changes) ? s.changes : [];
  if (!Array.isArray(s.changes) || (typeof s.baseline === 'number' && !changes.length)) out.push(`changes must list what this version changes from its baseline (${String(s.baseline)})`);
  for (const c of changes) if (!isObj(c) || !['choice', 'was', 'now', 'why'].every((k) => text(c[k]))) out.push('each change needs choice, was, now and why');
  const c = s.compatibility;
  if (!isObj(c) || !Array.isArray(c.breaking) || !strings(c.safe)) return [...out, 'compatibility needs breaking (what, callers and migration) and safe'];
  for (const b of c.breaking) if (!breakingOk(b)) out.push('each breaking entry needs what, at least one named caller, and migration: how those callers keep working');
  return out;
}

// Both sections exist exactly when a baseline does; whether a numbered baseline is approved is the judge's gate, which reads the approvals.
function baselineProblems(s: Record<string, unknown>): string[] {
  if (s.baseline === undefined) return s.changes === undefined && s.compatibility === undefined ? [] : ['changes and compatibility are measured against a baseline: name it in baseline (the last approved version, or "shipped" for a surface already in use), or leave both out of a greenfield contract'];
  if (s.baseline !== 'shipped' && !(Number.isInteger(s.baseline) && (s.baseline as number) >= 1)) return ['baseline must be the number of the last approved version of this contract, or "shipped" when none is approved and the surface is already in use'];
  return baselinedProblems(s);
}

/** Structure problems, all at once; whether refs resolve and the document is sound is the judge's gate. */
export function validateSpec(content: string): string[] {
  const s = parseSpec(content);
  if (typeof s === 'string') return [s];
  const out: string[] = [];
  if (!SPEC_KINDS.includes(s.kind)) out.push(`kind must be one of ${SPEC_KINDS.map((k) => `"${k}"`).join(', ')}`);
  if (!text(s.title)) out.push('title must be a non-empty string');
  if (!Number.isInteger(s.version) || s.version < 1) out.push('version must be a positive integer');
  out.push(...intentProblems(s.intent));
  if (s.kind === 'typescript' ? !text(s.document) : !isObj(s.document)) out.push('document must be the OpenAPI object, the JSON Schema, or for typescript the .d.ts source text');
  if (!Array.isArray(s.choices) || !s.choices.length) out.push('choices must list every decision the spec rests on');
  else out.push(...s.choices.flatMap(choiceProblems));
  out.push(...baselineProblems(s as unknown as Record<string, unknown>));
  return out;
}

export const summarizeSpec = (content: string) => {
  const s = parseSpec(content);
  return typeof s === 'string' ? '' : `${s.title}, version ${s.version}`;
};
