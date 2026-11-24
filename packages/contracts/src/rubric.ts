import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { createHash } from 'node:crypto';
import ts from 'typescript';
import { PASS_ONLY, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, factsOf, type Inputs, type Question as Asked, type SplitDecl, type Rubric } from '@quack/judge';
import { Approvals, Artifacts, sealedVersions, type Approval, type ArtifactsView, type RecordedSession, type SealedArtifact } from '@quack/sdk';
import { approvedSpec } from './approval.ts';
import type { Definition } from './definitions.ts';
import { kinds } from './kinds.ts';
import { exportsOf } from './typescript-rules.ts';
import { safeClaims, unnamedBreaks } from './breaks.ts';
import { FACT_LIMIT, type Fact } from './facts.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec, type Intent } from './spec.ts';

const slug = (heading: string) => heading.trim().toLowerCase();
/** Each guide rule's text by its anchor (a ### heading), up to the next heading. */
export const guideRules = (skill: string) => new Map([...skill.matchAll(/^###\s+(.+)\n([\s\S]*?)(?=^##|(?![\s\S]))/gm)].map((m) => [slug(m[1]), m[2].trim()]));
export const guideAnchors = (skill: string) => new Set(guideRules(skill).keys());

// A JSON pointer resolves when every segment names a key or index on the way down.
function pointerResolves(doc: unknown, pointer: string): boolean {
  if (pointer === '') return true;
  // A TypeScript document is source text; its pointer is "/" plus the name of an export.
  if (typeof doc === 'string') return exportsOf(ts.createSourceFile('contract.d.ts', doc, ts.ScriptTarget.ES2022, true)).some((e) => `/${e.name}` === pointer);
  let at: unknown = doc;
  for (const raw of pointer.split('/').slice(1)) {
    const key = raw.replaceAll('~1', '/').replaceAll('~0', '~');
    if (typeof at !== 'object' || at === null || !Object.hasOwn(at, key)) return false;
    at = (at as Record<string, unknown>)[key];
  }
  return true;
}

// A choice's pointer names the part of the document it decides; one into nothing decides nothing. Its whys are the writer's own account.
const strayPointers = (spec: ContractSpec) => spec.choices.filter((c) => !pointerResolves(spec.document, c.pointer)).map((c) => `${c.id}: pointer \`${c.pointer}\` is not in the document`);

type SpecState = { unresolved: string[]; breaking: Array<{ id: string; text: string; operation?: string; path?: string }>; expected: number; version: number; compatibility?: ContractSpec['compatibility']; baseline?: ContractSpec['baseline']; approved?: number };

/** What the judge reads beside the spec: the session's artifacts and approvals. */
type World = { kinds: NonNullable<ArtifactsView>['kinds']; approvals: Record<string, Approval> };

const escRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const mentions = (name: string) => new RegExp(`(?<![\\w$])${escRe(name)}(?![\\w$])`);
const textOf = (d: Definition) => (typeof d.definition === 'string' ? d.definition : JSON.stringify(d.definition));

/** `label` is a fact's id; `at` is where the entry sits, naming an item when it has no id; `whole` is a split string entry unsplit. */
type Entry = { label?: string; at: string; text: string; whole?: string };
type IntentList = 'consumers' | 'guarantees' | 'nonGoals';
// An entry is matched as one fact; a pre-v7 string entry over the fact limit holds several, so each of its sentences is matched alone.
function entriesOf(intent: Partial<Intent>, lists: IntentList[]): Entry[] {
  return lists.flatMap((list) => (intent[list] ?? []).flatMap((f: Fact, i): Entry[] => {
    if (typeof f !== 'string') return [{ label: f.id, at: `${list}[${i}]`, text: f.text }];
    const parts = f.length <= FACT_LIMIT ? [f] : f.split(/(?<=[.;])\s+(?=[A-Z`"(])/);
    return parts.map((text, n) => (parts.length > 1 ? { at: `${list}[${i}].${n + 1}`, text, whole: f } : { at: `${list}[${i}]`, text }));
  }));
}

const STOP = new Set(['that', 'this', 'with', 'from', 'have', 'which', 'what', 'when', 'will', 'does', 'into', 'also', 'than', 'then', 'there', 'their', 'each', 'only', 'more', 'over', 'such', 'both', 'been', 'they', 'them', 'where', 'while', 'about', 'after', 'before', 'never', 'other', 'same', 'must', 'every', 'none', 'unless', 'through', 'string', 'number', 'boolean', 'type', 'export', 'readonly', 'true', 'false', 'null', 'undefined', 'description', 'properties', 'required', 'object', 'array', 'items']);
const termRe = /[a-z][a-z0-9]{3,}/g;
// Whole identifiers and their camelCase parts both count, so "JudgeConfig" meets "judge config"; a plural meets its singular.
const termsOf = (s: string) => new Set([...(s.toLowerCase().match(termRe) ?? []), ...(s.replace(/([a-z0-9])([A-Z])/g, '$1 $2').toLowerCase().match(termRe) ?? [])].filter((w) => !STOP.has(w)).map((w) => w.replace(/(?<!s)s$/, '')));

// A guarantee's group key: its fact id, or for a grandfathered string its first words and a hash of its text, which an insert does not move.
const guaranteeKey = (e: Entry) => e.label ?? `${e.text.split(/\s+/).slice(0, 6).join(' ')} #${createHash('sha256').update(e.text).digest('hex').slice(0, 6)}`;

/** A part of the spec a guarantee may hold in: a definition, the rest of the document, or a decided choice. `stems` are what it is ranked by, computed once per split. */
type Part = { id: string; name?: string; stems: Set<string>; evidence: unknown; root?: true };

// "loader" meets "load" and "checked" meets "check".
const stemsOf = (s: string) => new Set([...termsOf(s)].map((t) => t.replace(/(?<=[a-z0-9]{4})(?:ing|ers?|ed)$/, '')));
const VERB_BEFORE = /\b(GET|PUT|POST|DELETE|OPTIONS|HEAD|PATCH|TRACE)\s+$/;
// A path, pointer, camelCase or dashed name cannot be an English word by chance; a plain word names a part only in backticks.
const codeLike = (name: string) => /[^A-Za-z]|.[A-Z]/.test(name);
// Path characters end a mention, so /sessions does not name /sessions/{key}.
const exactly = (name: string) => new RegExp(`(?<![\\w$/.{}-])${escRe(name)}(?![\\w$/{}-])`, 'g');

// A part the text names: an operation by its path, with no verb before it or its own; any other by its pointer, a choice's id or its
// name written as code, or a name that is no path as a whole word.
function namedIn(text: string, p: Part): boolean {
  const op = /^([A-Z]+) (\/\S*)$/.exec(p.id);
  if (op) return [...text.matchAll(exactly(op[2]))].some((m) => [undefined, op[1]].includes(VERB_BEFORE.exec(text.slice(0, m.index))?.[1]));
  const names = [p.id.replace(/^choice /, ''), p.name ?? ''].filter(Boolean);
  if (names.some((n) => text.includes(`\`${n}\``) || (codeLike(n) && exactly(n).test(text)))) return true;
  return !!p.name && !p.name.startsWith('/') && mentions(p.name).test(text);
}

// The parts a guarantee is paired with: every one it names, the `top` others sharing the most terms (the best few when it names none
// and none clear the bar), and the root, whose spec-wide rules (additionalProperties, security, scope) any may hold in.
function partsFor(text: string, parts: Part[], top: number, minShared: number): Part[] {
  const terms = stemsOf(text);
  const scored = parts.map((p, i) => ({ p, i, named: namedIn(text, p), s: [...p.stems].filter((t) => terms.has(t)).length }));
  const named = scored.filter((x) => x.named || x.p.root);
  const rest = scored.filter((x) => !named.includes(x)).sort((a, b) => b.s - a.s || a.i - b.i);
  const kept = rest.filter((x) => x.s >= minShared);
  const chosen = new Set([...named, ...(kept.length || named.some((x) => x.named) ? kept : rest).slice(0, top)]);
  return scored.filter((x) => chosen.has(x)).map((x) => x.p);
}

// The definitions, the rest of the document and the decided choices: a guarantee the code cannot show, such as layering or a
// budget the judge measures, is carried by the choice that decides it.
function partsOf(spec: Pick<ContractSpec, 'document' | 'choices'>, kind: ContractSpec['kind']): Part[] {
  const defs = kinds[kind].definitions(spec.document).filter((d) => /[\p{L}\p{N}]/u.test(textOf(d)));
  // A choice ranks by its decision alone: its whys quote the guide, and a quote many choices share would outrank the definitions.
  const choices = (spec.choices ?? []).map((c): Part => ({ id: `choice ${c.id}`, stems: stemsOf(c.decision), evidence: { decision: c.decision, why: c.why.map((w) => w.text) } }));
  // The root is paired with every guarantee, so its examples, which make nothing hold, stay out of it.
  const shown = (d: Definition) => (d.rest && typeof d.definition === 'object' && d.definition !== null ? { ...d.definition, examples: undefined } : d.definition);
  return [...defs.map((d): Part => ({ id: d.id, name: d.name, stems: stemsOf(textOf(d)), evidence: shown(d), ...(d.rest && { root: true as const }) })), ...choices];
}

// One item per (guarantee, part) pair, grouped by the guarantee, which is the pairs' shared context: the decision model reads one part
// at a time, and a guarantee's pairs pack into few requests. A grandfathered guarantee over the fact limit is judged sentence by sentence.
function guaranteePairs(inputs: Inputs, o: SplitDecl): Chunk[] {
  const [specInput, kindInput] = [o.from].flat();
  const spec = JSON.parse(inputs[specInput]) as Pick<ContractSpec, 'intent' | 'document' | 'choices'>;
  const parts = partsOf(spec, inputs[kindInput] as ContractSpec['kind']);
  return entriesOf(spec.intent, ['guarantees']).flatMap((g) => {
    const key = guaranteeKey(g);
    // A later sentence of a split entry may take its subject from an earlier one, so the whole entry goes along as context.
    const meta = { key, guarantee: g.text, ...(g.whole && { entry: g.whole }) };
    return partsFor(g.text, parts, o.top ?? 4, o.minShared ?? 2).map((p) => ({ id: `${key} ~ ${p.id}`, text: JSON.stringify(p.evidence), meta }));
  });
}
registerSplitter('contract.guaranteePairs', guaranteePairs);

// The named texts the judge reads: the facts code found, the spec and its kind.
async function inputsIn(sealed: SealedArtifact, w: World): Promise<Inputs> {
  const spec = parseSpec(sealed.content) as ContractSpec;
  const kind = kinds[spec.kind];
  // A revision of a failing attempt is a new sealed version but the same spec version; only passed ones can be proposed, so only they count.
  const cleared = sealedVersions(w, CONTRACT_KIND).slice(0, sealed.version - 1).filter((v) => (v.verdict as { passed?: boolean } | null)?.passed);
  // Breaks are against the approved spec only: a rejected draft is not a baseline, and shipped code has no earlier spec to diff.
  const base = approvedSpec(w.kinds, w.approvals, sealed.version);
  const prev = base && (parseSpec(base.content) as ContractSpec);
  const breaking = prev ? await kind.diff(prev.document, spec.document) : [];
  const { intent, choices, document, version, compatibility, baseline } = spec;
  const facts = { unresolved: strayPointers(spec), breaking: breaking.map(({ id, text, operation, path }) => ({ id, text, operation, path })), expected: cleared.length + 1, version, compatibility, baseline, approved: prev?.version };
  return {
    facts: JSON.stringify(facts),
    spec: JSON.stringify({ intent, choices, document }),
    kind: spec.kind,
  };
}

const yesNo = (instructions: string): Asked => ({ type: 'choice', instructions, options: { yes: 'Yes, this holds.', no: 'No, it does not hold.' } });

export const contractDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    guarantee: { split: { name: 'contract.guaranteePairs', from: ['spec', 'kind'], top: 4, minShared: 2 }, piece: 'part' },
  },
  checks: {
    // A guarantee holds when any part defines it, and the spec when every guarantee does. Clef only passes a pair, so a guarantee
    // fails only on the judge model's answers, with its reason; once one pair holds, the guarantee's others are not escalated.
    meets_intent: {
      scope: 'guarantee',
      decisionModel: PASS_ONLY,
      aggregate: { name: 'grouped', groupBy: 'key', inner: 'any', outer: 'all', pass: ['yes'], none: 'none of the {n} parts checked against this guarantee defines a mechanism for it' },
      question: yesNo('Does this part of the spec define a mechanism for the guarantee: a field, type, operation, header, status code or schema rule that makes it hold, or a behaviour the part\'s own text states? Answer no when the part only mentions the subject and leaves what makes the guarantee hold undefined.'),
    },
  },
};

/** What the checks read: the pointers that do not resolve, the breaking changes since the approved version, and the spec. */
export function specInputs() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Inputs> => {
    return inputsIn(sealed, {
      kinds: (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {},
      approvals: (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {},
    });
  };
}

/** The same inputs from a recording, as the session stood when the spec was sealed: later versions and approvals did not exist yet. */
export async function recordedSpecInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[CONTRACT_KIND]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the spec is not a version this session sealed, so what stood when it was judged is unknown');
  const kinds = Object.fromEntries(Object.entries(session.artifacts).map(([k, vs]) => [k, { versions: vs.filter((v) => v.sealedAt <= sealed.sealedAt) }]));
  const all = (session.docs['quack.approvals']?.items ?? {}) as unknown as Record<string, Approval>;
  const approvals = Object.fromEntries(Object.entries(all).filter(([, a]) => !a.verdict || a.verdict.at <= sealed.sealedAt));
  return inputsIn(sealed, { kinds, approvals });
}

const list = (items: string[]) => items.slice(0, 6).join('; ');

function versionMatches(_a: string, ev: Evidence): CodeResult {
  const { version, expected } = factsOf<SpecState>(ev);
  return { met: version === expected ? 1 : 0, detail: version === expected ? undefined : `version is ${version}; this is spec version ${expected} (one more than the last delivered), so set version to ${expected}` };
}

// The seal checks the baseline's shape; only here are the approvals known. "shipped" is the author's claim, which the reviewer checks.
function baselineMatches(_a: string, ev: Evidence): CodeResult {
  const { baseline, approved } = factsOf<SpecState>(ev);
  if (approved !== undefined) return baseline === approved ? { met: 1 } : { met: 0, detail: `version ${approved} of this contract is approved, so set baseline to ${approved} and write changes and compatibility against it` };
  if (typeof baseline !== 'number') return { met: 1 };
  return { met: 0, detail: `baseline is ${baseline}, but no version of this contract is approved: set it to "shipped" if the surface is already in use, else leave baseline, changes and compatibility out` };
}

function pointersResolve(_a: string, ev: Evidence): CodeResult {
  const { unresolved } = factsOf<SpecState>(ev);
  return { met: unresolved.length ? 0 : 1, detail: unresolved.length ? `${list(unresolved)}. Point each choice at the part of the document it decides.` : undefined };
}

// Break by break: an entry must name each place the kind's diff found, and a safe note that names an unnamed one is reported with it.
function breakingNamed(_a: string, ev: Evidence): CodeResult {
  const { breaking, compatibility } = factsOf<SpecState>(ev);
  if (!breaking.length) return { met: 1 };
  const named = compatibility?.breaking ?? [];
  const unnamed = unnamedBreaks(named, breaking);
  const out: string[] = [];
  if (!named.length || named.some((b) => !b.callers?.length)) out.push(`the kind's diff found breaking changes (${list(breaking.map((b) => b.text))}); compatibility.breaking must list each with the callers it breaks, found by reading the code that calls it`);
  else if (unnamed.length) out.push(`the kind's diff found breaks no compatibility.breaking entry names: ${unnamed.join(', ')}. Name each as a whole word in the what, callers or migration of the entry whose change causes it (one entry may name several), or add an entry for it`);
  for (const c of safeClaims(compatibility?.safe ?? [], breaking, unnamed)) out.push(`compatibility.safe[${c.note}] calls ${c.places.join(', ')} safe, but the kind's diff found ${c.places.length > 1 ? 'them' : 'it'} breaking`);
  return out.length ? { met: 0, detail: `${out.join('. ')}.` } : { met: 1 };
}

export const contractRubric: Rubric = {
  name: 'contract.spec',
  version: 17,
  // meets_intent is the one scored check and folds to a boolean, so the score is 0 or 1: any threshold above 0 means it must hold.
  threshold: 1,
  criteria: [
    { id: 'sound', checks: [
      { id: 'version_matches', label: 'The version number is wrong', weight: 'gate', code: versionMatches },
      { id: 'baseline_matches', label: 'The baseline is not the approved version', weight: 'gate', code: baselineMatches },
    ] },
    { id: 'justified', checks: [
      { id: 'pointers_resolve', label: "A choice's pointer isn't in the document", weight: 'gate', code: pointersResolve },
    ] },
    { id: 'compatible', checks: [
      { id: 'breaking_named', label: 'A breaking change names no caller', weight: 'gate', code: breakingNamed },
    ] },
    { id: 'matches_intent', checks: [
      { id: 'meets_intent', label: 'A guarantee has no mechanism in the document', weight: 1 },
    ] },
  ],
};
