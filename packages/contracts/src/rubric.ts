import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { isAbsolute, relative, sep } from 'node:path';
import { createHash } from 'node:crypto';
import ts from 'typescript';
import { registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, type Inputs, type Question as Asked, type SplitDecl, type Rubric } from '@quack/judge';
import { Approvals, Artifacts, Questions, type Approval, type Question, type RecordedSession, type SealedArtifact } from '@quack/sdk';
import { approvedSpec } from './approval.ts';
import type { Definition } from './definitions.ts';
import { kinds } from './kinds.ts';
import type { Finding } from './rest-rules.ts';
import { exportsOf } from './typescript-rules.ts';
import { safeClaims, unnamedBreaks } from './breaks.ts';
import { FACT_LIMIT, factProblems, type Fact } from './facts.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec, type Intent, type Why } from './spec.ts';

const slug = (heading: string) => heading.trim().toLowerCase();
/** Each guide rule's text by its anchor (a ### heading), up to the next heading. */
export const guideRules = (skill: string) => new Map([...skill.matchAll(/^###\s+(.+)\n([\s\S]*?)(?=^##|(?![\s\S]))/gm)].map((m) => [slug(m[1]), m[2].trim()]));
export const guideAnchors = (skill: string) => new Set(guideRules(skill).keys());
const anchorOf = (ref: string) => ref.replace(/^(rest|json-schema|typescript)#|^#/, '');

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

type Reader = (path: string) => Promise<string | undefined>;

// A misplaced ref has a clone or packages/ prefix too many or too few; dropping more could match an unrelated src/index.ts.
const MAX_DROPPED = 2;

// Where the file a wrong ref means is: its trailing segments at the workspace root or under a directory one or two levels down.
async function locate(read: Reader, roots: string[], path: string): Promise<string | undefined> {
  const parts = path.split('/').filter((p) => p && p !== '.' && p !== '..');
  for (let drop = 0; drop <= Math.min(MAX_DROPPED, parts.length - 2); drop++) {
    for (const root of ['', ...roots]) {
      const at = [root, ...parts.slice(drop)].filter(Boolean).join('/');
      if (at !== path && (await read(at)) !== undefined) return at;
    }
  }
  return undefined;
}

const CLONE = 'git clone --depth 1 -b <branch> <url> repo';
// The exact ref to write, taken from where the file is, else from the session's own clone directory.
async function missingFile(read: Reader, roots: string[], path: string, line: string): Promise<string> {
  const clone = roots[0];
  if (isAbsolute(path)) return `is an absolute path; refs are paths from the workspace root, as "${clone ?? 'repo'}/<path in the clone>:${line}"`;
  const at = await locate(read, roots, path);
  // app/ was the session image's own tree, which refs carried from older versions still cite; only the workspace resolves now.
  if (path.startsWith('app/')) return `cites app/, the old session image's tree, which no longer resolves: re-anchor it to the clone, as "${at ?? `${clone ?? 'repo'}/${path.slice(4)}`}:${line}"${clone ? '' : `, once the repository is cloned into repo/ (${CLONE})`}`;
  if (at) return `names no file in the workspace; the file is at ${at}, so cite it as "${at}:${line}"`;
  if (!clone) return `names no file in the workspace, which holds no clone: clone the repository into repo/ (${CLONE}) and cite a file at ${path} in it as "repo/${path}:${line}"`;
  const rel = path.startsWith(`${clone}/`) ? path.slice(clone.length + 1) : path;
  return `names no file in the workspace. Refs are paths from the workspace root and your clone is ${clone}/, so a file at ${rel} in the clone is cited as "${clone}/${rel}:${line}"; read the clone to find where the file is`;
}

// Why a code ref fails, or undefined when it resolves.
async function codeRefProblem(read: Reader | undefined, roots: string[], ref: string): Promise<string | undefined> {
  const m = /^(.+?):(\d+)$/.exec(ref);
  if (!m || !read) return 'is not path:line';
  const text = await read(m[1]);
  if (text === undefined) return missingFile(read, roots, m[1], m[2]);
  const lines = text.split('\n').length;
  return Number(m[2]) >= 1 && Number(m[2]) <= lines ? undefined : `is past the end of ${m[1]} (${lines} lines)`;
}

// A guide ref names any ## or ### heading, matched without case and with a dash read as a space, as agents write anchors.
const headingKey = (s: string) => s.trim().toLowerCase().replace(/[\s-]+/g, ' ');
const guideHeadings = (skill: string) => [...skill.matchAll(/^#{2,3}\s+(.+)$/gm)].map((m) => m[1].trim());

async function whyProblem(w: Why, headings: string[], answered: Set<string>, world: World): Promise<string | undefined> {
  const ref = anchorOf(w.ref);
  // An answer's ref is the question as asked or its ask_question id; "request" is the person's first description, which is not a question.
  if (w.source === 'answer') return ref === 'request' || answered.has(w.ref) ? undefined : 'does not resolve';
  if (w.source === 'guide') return headings.some((h) => headingKey(h) === headingKey(ref)) ? undefined : `does not resolve; the guide's headings are ${headings.map((h) => `"${h}"`).join(', ')}`;
  return codeRefProblem(world.read, world.roots, w.ref);
}

async function unresolvedLinks(spec: ContractSpec, skill: string, answered: Set<string>, world: World): Promise<string[]> {
  const headings = guideHeadings(skill);
  const out: string[] = [];
  for (const c of spec.choices) {
    // A JSON pointer's ~1 escapes read as strikethrough in Markdown, so the note quotes it as code.
    if (!pointerResolves(spec.document, c.pointer)) out.push(`${c.id}: pointer \`${c.pointer}\` is not in the document`);
    for (const w of c.why) {
      const problem = await whyProblem(w, headings, answered, world);
      if (problem) out.push(`${c.id}: ${w.source} ref "${w.ref}" ${problem}`);
    }
  }
  return out;
}

type SpecState = { findings: Finding[]; unresolved: string[]; undecomposed?: string[]; breaking: Array<{ id: string; text: string; operation?: string; path?: string }>; expected: number; version: number; compatibility?: ContractSpec['compatibility']; baseline?: ContractSpec['baseline']; approved?: number };
const state = (ev: Evidence) => ev.facts[0].state.facts as unknown as SpecState;

/** What the judge reads beside the spec: the session's artifacts and approvals, the questions answered, its workspace's files, and the
 * directories a ref's file may sit under, the clone first. */
type World = { kinds: Record<string, { versions: SealedArtifact[] }>; approvals: Record<string, Approval>; answered: Set<string>; read: Reader | undefined; roots: string[] };

const escRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const mentions = (name: string) => new RegExp(`(?<![\\w$])${escRe(name)}(?![\\w$])`);
const textOf = (d: Definition) => (typeof d.definition === 'string' ? d.definition : JSON.stringify(d.definition));

/** `label` is a fact's id; `at` is where the entry sits, naming an item when it has no id. */
type Entry = { label?: string; at: string; text: string };
type IntentList = 'consumers' | 'guarantees' | 'nonGoals';
// An entry is matched as one fact; a pre-v7 string entry over the fact limit holds several, so each of its sentences is matched alone.
function entriesOf(intent: Partial<Intent>, lists: IntentList[]): Entry[] {
  return lists.flatMap((list) => (intent[list] ?? []).flatMap((f: Fact, i): Entry[] => {
    if (typeof f !== 'string') return [{ label: f.id, at: `${list}[${i}]`, text: f.text }];
    const parts = f.length <= FACT_LIMIT ? [f] : f.split(/(?<=[.;])\s+(?=[A-Z`"(])/);
    return parts.map((text, n) => ({ at: parts.length > 1 ? `${list}[${i}].${n + 1}` : `${list}[${i}]`, text }));
  }));
}

const STOP = new Set(['that', 'this', 'with', 'from', 'have', 'which', 'what', 'when', 'will', 'does', 'into', 'also', 'than', 'then', 'there', 'their', 'each', 'only', 'more', 'over', 'such', 'both', 'been', 'they', 'them', 'where', 'while', 'about', 'after', 'before', 'never', 'other', 'same', 'must', 'every', 'none', 'unless', 'through', 'string', 'number', 'boolean', 'type', 'export', 'readonly', 'true', 'false', 'null', 'undefined', 'description', 'properties', 'required', 'object', 'array', 'items']);
const termRe = /[a-z][a-z0-9]{3,}/g;
// Whole identifiers and their camelCase parts both count, so "JudgeConfig" meets "judge config"; a plural meets its singular.
const termsOf = (s: string) => new Set([...(s.toLowerCase().match(termRe) ?? []), ...(s.replace(/([a-z0-9])([A-Z])/g, '$1 $2').toLowerCase().match(termRe) ?? [])].filter((w) => !STOP.has(w)).map((w) => w.replace(/(?<!s)s$/, '')));

// A guarantee's group key: its fact id, or for a grandfathered string its first words and a hash of its text, which an insert does not move.
const guaranteeKey = (e: Entry) => e.label ?? `${e.text.split(/\s+/).slice(0, 6).join(' ')} #${createHash('sha256').update(e.text).digest('hex').slice(0, 6)}`;

/** A part of the spec a guarantee may hold in: a definition, the rest of the document, or a decided choice. */
type Part = { id: string; name?: string; text: string; evidence: unknown; root?: true };

// The parts a guarantee is checked against, best first: one it names, then by shared terms; with none above the bar, the best few anyway,
// so every guarantee is asked. The root holds spec-wide rules (additionalProperties, security, the stated scope), so it is always asked.
function partsFor(text: string, parts: Part[], top: number, minShared: number): Part[] {
  const terms = termsOf(text);
  const scored = parts.map((p, i) => {
    const shared = [...termsOf(p.text)].filter((t) => terms.has(t)).length;
    return { p, i, shared, s: (p.name && mentions(p.name).test(text) ? 1000 : 0) + shared };
  });
  const ranked = [...scored].sort((a, b) => b.s - a.s || a.i - b.i);
  const kept = ranked.filter((x) => x.s >= minShared);
  const best = (kept.length ? kept : ranked).slice(0, top);
  const root = scored.find((x) => x.p.root);
  // Order evidence by shared terms so the most relevant part comes first: a name the guarantee only
  // happens to use as a word ("session's") keeps a part in, not first.
  return (root && !best.includes(root) ? [...best, root] : best).sort((a, b) => b.shared - a.shared).map((x) => x.p);
}

// The definitions, the rest of the document and the decided choices: a guarantee the code cannot show, such as layering or a
// budget the judge measures, is carried by the choice that decides it.
function partsOf(spec: Pick<ContractSpec, 'document' | 'choices'>, kind: ContractSpec['kind']): Part[] {
  const defs = kinds[kind].definitions(spec.document).filter((d) => /[\p{L}\p{N}]/u.test(textOf(d)));
  const choices = (spec.choices ?? []).map((c): Part => {
    const evidence = { decision: c.decision, why: c.why.map((w) => w.text) };
    return { id: `choice ${c.id}`, text: JSON.stringify(evidence), evidence };
  });
  // The root goes with every promise, so its examples, which make nothing hold, stay out of the evidence.
  const shown = (d: Definition) => (d.rest && typeof d.definition === 'object' && d.definition !== null ? { ...d.definition, examples: undefined } : d.definition);
  return [...defs.map((d): Part => ({ id: d.id, name: d.name, text: textOf(d), evidence: shown(d), ...(d.rest && { root: true as const }) })), ...choices];
}

// One item per promise; its evidence is the parts that may make it hold (named or sharing the most terms, the decided choices, the root),
// most relevant first, so one judge-model question per promise reads them together.
function promiseItems(inputs: Inputs, o: SplitDecl, promisesOf: (intent: Partial<Intent> & { purpose: string }) => Entry[]): Chunk[] {
  const [specInput, kindInput] = [o.from].flat();
  const spec = JSON.parse(inputs[specInput]) as Pick<ContractSpec, 'intent' | 'document' | 'choices'>;
  const parts = partsOf(spec, inputs[kindInput] as ContractSpec['kind']);
  return promisesOf(spec.intent).map((g) => {
    const evidence = partsFor(g.text, parts, o.top ?? 4, o.minShared ?? 2).map((p) => ({ part: p.id, text: p.evidence }));
    return { id: guaranteeKey(g), text: g.text, meta: { evidence } };
  });
}
registerSelector('contract.evidence', (_i, c) => JSON.stringify((c.meta as { evidence: unknown[] }).evidence));

// A grandfathered guarantee over the fact limit holds several facts, and no one part may give them all: each sentence is a guarantee.
// With none, the check has no items and counts toward neither side.
registerSplitter('contract.guarantees', (inputs, o) => promiseItems(inputs, o, (intent) => entriesOf(intent, ['guarantees'])));

// The named texts the judge reads: the facts code found, the spec and its kind.
async function inputsIn(sealed: SealedArtifact, w: World): Promise<Inputs> {
  const spec = parseSpec(sealed.content) as ContractSpec;
  const kind = kinds[spec.kind];
  // A revision of a failing attempt is a new sealed version but the same spec version; only passed ones can be proposed, so only they count.
  const cleared = (w.kinds[CONTRACT_KIND]?.versions ?? []).slice(0, sealed.version - 1).filter((v) => (v.verdict as { passed?: boolean } | null)?.passed);
  // Breaks are against the approved spec only: a rejected draft is not a baseline, and shipped code has no earlier spec to diff.
  const base = approvedSpec(w.kinds, w.approvals, sealed.version);
  const prev = base && (parseSpec(base.content) as ContractSpec);
  const [findings, unresolved, breaking] = await Promise.all([
    kind.check(spec.document),
    unresolvedLinks(spec, kind.skill, w.answered, w),
    prev ? kind.diff(prev.document, spec.document) : [],
  ]);
  const { intent, choices, document, version, compatibility, baseline } = spec;
  const facts = { findings, unresolved, undecomposed: factProblems(spec, prev), breaking: breaking.map(({ id, text, operation, path }) => ({ id, text, operation, path })), expected: cleared.length + 1, version, compatibility, baseline, approved: prev?.version };
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
    // One item per guarantee; the evidence is the parts it may hold in, at most four plus the root.
    guarantee: { split: { name: 'contract.guarantees', from: ['spec', 'kind'], top: 4, minShared: 2 }, piece: 'guarantee', context: { evidence: { name: 'contract.evidence' } } },
  },
  checks: {
    // The judge model answers every promise, with a reason; the decision model serves one request at a time, and many pair questions
    // queued behind it made the round slower than the one question they replaced.
    meets_intent: { scope: 'guarantee', aggregate: { name: 'all', pass: ['yes'] }, decisionModel: false, question: yesNo('Does the evidence shown make this guarantee hold, through a field, type, operation, header, status code or schema rule it defines, or a behaviour its own text states?') },
  },
};

// A question's record is keyed "<task>:<ask_question id>", so an answer can be cited by its text or its id.
const answeredIn = (items: Record<string, Question>, by = Infinity) =>
  new Set(Object.entries(items).filter(([, q]) => q.answer && q.answer.at <= by).flatMap(([key, q]) => [q.question, key.slice(key.indexOf(':') + 1)]));

/** What the checks read: the mechanical findings, the links that do not resolve, the breaking changes since the previous version, and the spec. */
export function specInputs() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Inputs> => {
    return inputsIn(sealed, {
      kinds: (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {},
      approvals: (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {},
      answered: answeredIn((await api.snapshot(Questions, api.conversationId, ctx))?.items ?? {}),
      ...(api.env ? await workspaceOf(api.env, ctx) : { read: undefined, roots: [] }),
    });
  };
}

const within = (root: string, path: string) => { const rel = relative(root, path); return rel !== '' && rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel); };

// Reads only inside the workspace, as a recording's reader does: an absolute path, a .. or a symlink out of it names no file there.
async function workspaceOf(env: NonNullable<ToolExecutionApi['env']>, ctx: Context): Promise<Pick<World, 'read' | 'roots'>> {
  const root = await env.canonicalPath(env.cwd, ctx);
  const read = async (path: string) => {
    if (isAbsolute(path)) return undefined;
    const real = await env.canonicalPath(path, ctx);
    if (!root.ok || !real.ok || !within(root.value, real.value)) return undefined;
    const r = await env.readTextFile(real.value, ctx);
    return r.ok ? r.value : undefined;
  };
  const entries = async (path: string) => { const listed = await env.listDir(path, ctx); return listed.ok ? listed.value : []; };
  const dirs = async (path: string) => (await entries(path)).filter((f) => f.kind === 'directory' && !f.name.startsWith('.')).map((f) => f.name).sort();
  // A clone is a git repository at the root, never a stray __pycache__ or artifacts/; its subdirectories (repo/packages) are where a ref may have lost a level.
  const top = await dirs('.');
  const isRepo = await Promise.all(top.map(async (d) => (await entries(d)).some((f) => f.name === '.git')));
  const clones = top.filter((_, i) => isRepo[i]);
  const inner = await Promise.all(clones.map(async (c) => (await dirs(c)).map((d) => `${c}/${d}`)));
  return { read, roots: [...clones, ...inner.flat()] };
}

/** The same inputs from a recording, as the session stood when the spec was sealed: later versions, approvals and answers did not exist yet. */
export async function recordedSpecInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[CONTRACT_KIND]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the spec is not a version this session sealed, so what stood when it was judged is unknown');
  if (!session.readFile) throw new Error('the recording keeps no workspace to resolve code refs in');
  const kinds = Object.fromEntries(Object.entries(session.artifacts).map(([k, vs]) => [k, { versions: vs.filter((v) => v.sealedAt <= sealed.sealedAt) }]));
  const all = (session.docs['quack.approvals']?.items ?? {}) as unknown as Record<string, Approval>;
  const approvals = Object.fromEntries(Object.entries(all).filter(([, a]) => !a.verdict || a.verdict.at <= sealed.sealedAt));
  const questions = (session.docs['quack.questions']?.items ?? {}) as unknown as Record<string, Question>;
  // shortcut: a recording reads files but cannot list them, so its clone is taken to be repo/ as the prompt says; list it if replays need others.
  return inputsIn(sealed, { kinds, approvals, answered: answeredIn(questions, sealed.sealedAt), read: session.readFile, roots: ['repo', 'repo/packages'] });
}

const list = (items: string[]) => items.slice(0, 6).join('; ');

function documentSound(_a: string, ev: Evidence): CodeResult {
  const { findings } = state(ev);
  return { met: findings.length ? 0 : 1, detail: findings.length ? `the document breaks the guide: ${list(findings.map((f) => `${f.message} (${f.path})`))}` : undefined };
}

function versionMatches(_a: string, ev: Evidence): CodeResult {
  const { version, expected } = state(ev);
  return { met: version === expected ? 1 : 0, detail: version === expected ? undefined : `version is ${version}; this is spec version ${expected} (one more than the last delivered), so set version to ${expected}` };
}

// The seal checks the baseline's shape; only here are the approvals known. "shipped" is the author's claim, which the reviewer checks.
function baselineMatches(_a: string, ev: Evidence): CodeResult {
  const { baseline, approved } = state(ev);
  if (approved !== undefined) return baseline === approved ? { met: 1 } : { met: 0, detail: `version ${approved} of this contract is approved, so set baseline to ${approved} and write changes and compatibility against it` };
  if (typeof baseline !== 'number') return { met: 1 };
  return { met: 0, detail: `baseline is ${baseline}, but no version of this contract is approved: set it to "shipped" if the surface is already in use, else leave baseline, changes and compatibility out` };
}

function factsDecomposed(_a: string, ev: Evidence): CodeResult {
  const { undecomposed = [] } = state(ev);
  return { met: undecomposed.length ? 0 : 1, detail: undecomposed.length ? `${list(undecomposed)}.` : undefined };
}

function linksResolve(_a: string, ev: Evidence): CodeResult {
  const { unresolved } = state(ev);
  return { met: unresolved.length ? 0 : 1, detail: unresolved.length ? `${list(unresolved)}. Cite an answered question by its ask_question id or its text exactly as asked, a guide heading, or an existing path:line.` : undefined };
}

// Break by break: an entry must name each place the kind's diff found, and a safe note that names an unnamed one is reported with it.
function breakingNamed(_a: string, ev: Evidence): CodeResult {
  const { breaking, compatibility } = state(ev);
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
  version: 13,
  // meets_intent is the one scored check and folds to a boolean, so the score is 0 or 1: any threshold above 0 means it must hold.
  threshold: 1,
  criteria: [
    { id: 'sound', checks: [
      { id: 'document_clean', label: 'The document breaks a guide rule', weight: 'gate', code: documentSound },
      { id: 'version_matches', label: 'The version number is wrong', weight: 'gate', code: versionMatches },
      { id: 'baseline_matches', label: 'The baseline is not the approved version', weight: 'gate', code: baselineMatches },
      { id: 'facts_decomposed', label: 'A guarantee or non-goal is not one checkable fact', weight: 'gate', code: factsDecomposed },
    ] },
    { id: 'justified', checks: [
      { id: 'links_resolve', label: "A choice's link doesn't resolve", weight: 'gate', code: linksResolve },
    ] },
    { id: 'compatible', checks: [
      { id: 'breaking_named', label: 'A breaking change names no caller', weight: 'gate', code: breakingNamed },
    ] },
    { id: 'matches_intent', checks: [
      { id: 'meets_intent', label: 'A guarantee has no mechanism in the document', weight: 1 },
    ] },
  ],
};
