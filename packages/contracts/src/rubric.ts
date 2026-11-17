import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { isAbsolute, relative, sep } from 'node:path';
import ts from 'typescript';
import { registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, type Inputs, type Question as Asked, type SplitDecl, type Rubric } from '@quack/judge';
import { Approvals, Artifacts, Questions, type Approval, type Question, type RecordedSession, type SealedArtifact } from '@quack/sdk';
import { approvedSpec } from './approval.ts';
import type { Definition } from './definitions.ts';
import { kinds } from './kinds.ts';
import type { Finding } from './rest-rules.ts';
import { exportsOf } from './typescript-rules.ts';
import { commentProblems } from './answered.ts';
import { safeClaims, unnamedBreaks } from './breaks.ts';
import { FACT_LIMIT, factProblems, type Fact } from './facts.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec, type Intent } from './spec.ts';

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

async function unresolvedLinks(spec: ContractSpec, anchors: Set<string>, answered: Set<string>, world: World): Promise<string[]> {
  const out: string[] = [];
  for (const c of spec.choices) {
    // A JSON pointer's ~1 escapes read as strikethrough in Markdown, so the note quotes it as code.
    if (!pointerResolves(spec.document, c.pointer)) out.push(`${c.id}: pointer \`${c.pointer}\` is not in the document`);
    for (const w of c.why) {
      const ref = anchorOf(w.ref);
      // An answer's ref is the question as asked or its ask_question id; "request" is the person's first description, which is not a question.
      const problem = w.source === 'answer' ? (ref === 'request' || answered.has(w.ref) ? undefined : 'does not resolve') : w.source === 'guide' ? (anchors.has(ref) ? undefined : 'does not resolve') : await codeRefProblem(world.read, world.roots, w.ref);
      if (problem) out.push(`${c.id}: ${w.source} ref "${w.ref}" ${problem}`);
    }
  }
  return out;
}

type SpecState = { findings: Finding[]; unresolved: string[]; undecomposed?: string[]; unanswered?: string[]; breaking: Array<{ id: string; text: string; operation?: string; path?: string }>; expected: number; version: number; compatibility?: ContractSpec['compatibility']; baseline?: ContractSpec['baseline']; approved?: number };
const state = (ev: Evidence) => ev.facts[0].state.facts as unknown as SpecState;

/** What the judge reads beside the spec: the session's artifacts and approvals, the questions answered, its workspace's files, and the
 * directories a ref's file may sit under, the clone first. */
type World = { kinds: Record<string, { versions: SealedArtifact[] }>; approvals: Record<string, Approval>; answered: Set<string>; read: Reader | undefined; roots: string[] };

const escRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const mentions = (name: string) => new RegExp(`(?<![\\w$])${escRe(name)}(?![\\w$])`);
// The choices that decide a definition: their pointer is at or under it, or their decision names it; what the person or code asked for shows there.
function decisionsOn(d: Definition, choices: ContractSpec['choices']) {
  const named = d.name && mentions(d.name);
  const on = (c: ContractSpec['choices'][number]) => (d.pointer && (c.pointer === d.pointer || c.pointer.startsWith(`${d.pointer}/`))) || (named && named.test(c.decision));
  return choices.filter(on).map((c) => c.id);
}

// The definitions that use one: a local $ref to it, or for TypeScript its name in their text; a shared shape is asked for by its users.
function usersOf(d: Definition, defs: Definition[]): string[] {
  const named = typeof d.definition === 'string' && d.name && mentions(d.name);
  const uses = (o: Definition) => (named ? typeof o.definition === 'string' && named.test(o.definition) : !!d.pointer && JSON.stringify(o.definition).includes(`"#${d.pointer}"`));
  return defs.filter((o) => o !== d && uses(o)).map((o) => o.id);
}

type Covered = { decided: string[]; usedBy: string[] };
const textOf = (d: Definition) => (typeof d.definition === 'string' ? d.definition : JSON.stringify(d.definition));

function definitionsIn(inputs: Inputs, o: SplitDecl) {
  const [specInput, kindInput] = [o.from].flat();
  const { choices, document } = JSON.parse(inputs[specInput]) as Pick<ContractSpec, 'choices' | 'document'>;
  const defs = kinds[inputs[kindInput] as ContractSpec['kind']].definitions(document);
  // The rest of the document still counts as a user, but is no definition of its own: a header cannot add or implement anything.
  // A definition with no letter or digit ({} or []) is no item of the judge's either, so both checks leave it out alike.
  const judged = (d: Definition) => !d.rest && /[\p{L}\p{N}]/u.test(textOf(d));
  return defs.filter(judged).map((d) => ({ d, covered: { decided: decisionsOn(d, choices), usedBy: usersOf(d, defs) } satisfies Covered }));
}

// A contract document splits on its kind's own structure: each top-level definition, carrying the choices that decide it and the definitions that use it.
const definitionChunks = (inputs: Inputs, o: SplitDecl): Chunk[] => definitionsIn(inputs, o).map(({ d, covered }) => ({ id: d.id, key: d.name, text: textOf(d), meta: covered }));
registerSplitter('contract.definitions', definitionChunks);

// The question's own terms count a definition a decided choice covers, or a shape another definition uses, as asked for: code settles
// it, and an unasked definition is flagged where nothing uses it.
const coveredBy = (c: Covered) => c.decided.length + c.usedBy.length;
registerSelector('contract.covered', (_i, c) => ({ value: '', matches: coveredBy(c.meta as Covered) }));

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

// The entries that plausibly concern a definition, best first: one that names it outranks any count of shared terms.
function relatedEntries(name: string | undefined, text: string, entries: Entry[], top: number, minShared: number): Entry[] {
  const terms = termsOf(text);
  const named = name ? mentions(name) : undefined;
  const scored = entries.map((e, i) => ({ e, i, s: (named?.test(e.text) ? 1000 : 0) + [...termsOf(e.text)].filter((t) => terms.has(t)).length }));
  return scored.filter((x) => x.s >= minShared).sort((a, b) => b.s - a.s || a.i - b.i).slice(0, top).map((x) => x.e);
}

const intentOf = (inputs: Inputs, from: string) => (JSON.parse(inputs[from]) as Pick<ContractSpec, 'intent'>).intent;
const shown = (e: Entry) => (e.label ? `${e.label}: ${e.text}` : e.text);
type EntriesDecl = { from: string; lists: IntentList[]; top?: number; minShared?: number };

// Only the consumers and guarantees that relate, so an edit elsewhere in the intent leaves the item's evidence as it was;
// with none related, all of them, since a "no" on too little context would flag what the intent does ask for.
registerSelector('contract.intentEntries', (inputs, c, o) => {
  const { from, lists, top = 3, minShared = 2 } = o as unknown as EntriesDecl;
  const all = entriesOf(intentOf(inputs, from), lists);
  const related = relatedEntries(c.key, c.text, all, top, minShared);
  return JSON.stringify((related.length ? related : all).map(shown));
});

// One item per definition code did not settle and non-goal that plausibly concerns it. Every other definition is one item code settles
// as held, so the share is over the definitions (a definition with two pairs counts twice) and one flag never costs the whole weight.
function nonGoalPairs(inputs: Inputs, o: SplitDecl): Chunk[] {
  const goals = entriesOf(intentOf(inputs, [o.from].flat()[0]), ['nonGoals']);
  return definitionsIn(inputs, o).flatMap(({ d, covered }) => {
    const pairs = coveredBy(covered) ? [] : relatedEntries(d.name, textOf(d), goals, o.top ?? 2, o.minShared ?? 1);
    if (!pairs.length) return [{ id: d.id, text: d.id, meta: { held: 1 } }];
    return pairs.map((g) => ({ id: `${d.id} ~ ${g.label ?? g.at}`, text: JSON.stringify({ definition: d.definition, nonGoal: g.text }), meta: { held: 0 } }));
  });
}
registerSplitter('contract.nonGoalPairs', nonGoalPairs);
registerSelector('contract.held', (_i, c) => ({ value: '', matches: Number(c.meta?.held ?? 0) }));

// The named texts the judge reads: the facts code found, the spec, its choices with the guide rules each cites, and the kind's guide.
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
    unresolvedLinks(spec, guideAnchors(kind.skill), w.answered, w),
    prev ? kind.diff(prev.document, spec.document) : [],
  ]);
  const { intent, choices, document, version, compatibility, baseline } = spec;
  const facts = { findings, unresolved, undecomposed: factProblems(spec, prev), unanswered: commentProblems(spec.changes, sealed.sealedAt, w.approvals, w.kinds), breaking: breaking.map(({ id, text, operation, path }) => ({ id, text, operation, path })), expected: cleared.length + 1, version, compatibility, baseline, approved: prev?.version };
  const cites = (c: ContractSpec['choices'][number]) => [...new Set(c.why.filter((x) => x.source === 'guide').map((x) => anchorOf(x.ref)))];
  return {
    facts: JSON.stringify(facts),
    spec: JSON.stringify({ intent, choices, document }),
    kind: spec.kind,
    choices: JSON.stringify(choices.map((c) => ({ ...c, cites: cites(c) }))),
    guide: JSON.stringify(Object.fromEntries(guideRules(kind.skill))),
  };
}

// A pitfall asks about the departure and holds on "no": worded as the claim cost Clef confidence (61% of choices, 74% of definitions at 0.8; 97%, 98% this way).
// adds_unasked is the owner's exception, worded as the claim and holding on "yes"; the replay's clef and esc columns track its cost.
const yesNo = (instructions: string): Asked => ({ type: 'choice', instructions, options: { yes: 'Yes, this holds.', no: 'No, it does not hold.' } });

// Every choice is judged as a design decision against the whole guide, sent once per request; most cite only answers and code,
// and a departure nobody cited is what the check is for. The document itself is meets_intent's to read.
export const contractDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    spec: { split: { name: 'whole', from: 'spec' }, piece: 'spec' },
    choice: { split: { name: 'jsonItems', from: 'choices' }, piece: 'choice', context: { guide: { name: 'json', from: 'guide' } } },
    // The kind is the one context every definition and pair shares, so the decision model takes them packed, a request per cap's worth.
    definition: {
      split: { name: 'contract.definitions', from: ['spec', 'kind'] },
      piece: 'definition',
      // The purpose is the intent's own statement of scope, shared by every definition, so it packs and only a purpose edit re-asks them all.
      context: { kind: { name: 'whole', from: 'kind' }, purpose: { name: 'json', from: 'spec', pointer: '/intent/purpose' }, covered: { name: 'contract.covered' }, consumersAndGuarantees: { name: 'contract.intentEntries', from: 'spec', lists: ['consumers', 'guarantees'], top: 3, minShared: 2 } },
    },
    nonGoal: { split: { name: 'contract.nonGoalPairs', from: ['spec', 'kind'], top: 2, minShared: 1 }, piece: 'pair', context: { kind: { name: 'whole', from: 'kind' }, held: { name: 'contract.held' } } },
  },
  // A pitfall costs its weight times the share of items flagged, so one flag among many costs little and fails nothing.
  // A definition is unasked when no consumer or guarantee names or implies it, or when it implements a non-goal; the two halves are two checks.
  checks: {
    meets_intent: { scope: 'spec', aggregate: { name: 'all' }, question: { type: 'bool', instructions: 'Does the document deliver the purpose in the intent, and does every guarantee in the intent have a mechanism in the document (a header, a status code, a schema rule)?' } },
    contradicts_guide: { scope: 'choice', aggregate: { name: 'share', pass: ['no'] }, question: yesNo('Does this choice contradict a guide rule without its why giving a reason from the person or the code? A planned change to current code is not a contradiction.') },
    adds_unasked: { scope: 'definition', aggregate: { name: 'share', by: 'either', pass: ['yes'] }, question: yesNo('Is this definition named or implied by the purpose, or by one of the consumers or guarantees shown with it, or does one of them need it in order to work?') },
    implements_non_goal: { scope: 'nonGoal', aggregate: { name: 'share', by: 'either', pass: ['no'] }, question: yesNo('Does this definition implement the non-goal shown with it?') },
  },
};

// A question's record is keyed "<task>:<ask_question id>", so an answer can be cited by its text or its id.
const answeredIn = (items: Record<string, Question>, by = Infinity) =>
  new Set(Object.entries(items).filter(([, q]) => q.answer && q.answer.at <= by).flatMap(([key, q]) => [q.question, key.slice(key.indexOf(':') + 1)]));

/** What the checks read: the mechanical findings, the links that do not resolve, the breaking changes since the previous version, and the spec with its guide. */
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

function commentsAnswered(_a: string, ev: Evidence): CodeResult {
  const { unanswered = [] } = state(ev);
  return { met: unanswered.length ? 0 : 1, detail: unanswered.length ? unanswered.join('\n') : undefined };
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
  version: 9,
  threshold: 0.8,
  criteria: [
    { id: 'sound', checks: [
      { id: 'document_clean', label: 'The document breaks a guide rule', weight: 'gate', code: documentSound },
      { id: 'version_matches', label: 'The version number is wrong', weight: 'gate', code: versionMatches },
      { id: 'baseline_matches', label: 'The baseline is not the approved version', weight: 'gate', code: baselineMatches },
      { id: 'facts_decomposed', label: 'A guarantee or non-goal is not one checkable fact', weight: 'gate', code: factsDecomposed },
      { id: 'comments_answered', label: 'A sent review comment is neither addressed nor answered', weight: 'gate', code: commentsAnswered },
    ] },
    { id: 'justified', checks: [
      { id: 'links_resolve', label: "A choice's link doesn't resolve", weight: 'gate', code: linksResolve },
    ] },
    { id: 'compatible', checks: [
      { id: 'breaking_named', label: 'A breaking change names no caller', weight: 'gate', code: breakingNamed },
    ] },
    { id: 'matches_intent', checks: [
      { id: 'meets_intent', label: "The spec doesn't deliver the intent", weight: 4 },
      { id: 'contradicts_guide', label: 'A choice contradicts the guide', weight: -3 },
      // The two halves of "unasked" share the weight it had alone.
      { id: 'adds_unasked', label: 'The spec adds what nobody asked for', weight: -2 },
      { id: 'implements_non_goal', label: 'The spec implements a non-goal', weight: -1 },
    ] },
  ],
};
