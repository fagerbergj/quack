import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { isAbsolute, relative, sep } from 'node:path';
import ts from 'typescript';
import { registerSplitter, type CodeResult, type Declaration, type Evidence, type Inputs, type Question as Asked, type SplitDecl, type Rubric } from '@quack/judge';
import { Approvals, Artifacts, Questions, type Approval, type Question, type RecordedSession, type SealedArtifact } from '@quack/sdk';
import { approvedSpec } from './approval.ts';
import type { Definition } from './definitions.ts';
import { kinds } from './kinds.ts';
import type { Finding } from './rest-rules.ts';
import { exportsOf } from './typescript-rules.ts';
import { CONTRACT_KIND, parseSpec, type ContractSpec } from './spec.ts';

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

// The exact ref to write, taken from where the file is, else from the session's own clone directory.
async function missingFile(read: Reader, roots: string[], path: string, line: string): Promise<string> {
  const clone = roots[0];
  if (isAbsolute(path)) return `is an absolute path; refs are paths from the workspace root, as "${clone ?? 'repo'}/<path in the clone>:${line}"`;
  const at = await locate(read, roots, path);
  if (at) return `names no file in the workspace; the file is at ${at}, so cite it as "${at}:${line}"`;
  if (!clone) return `names no file in the workspace, which holds no clone: clone the repository into repo/ (git clone --depth 1 -b <branch> <url> repo) and cite a file at ${path} in it as "repo/${path}:${line}"`;
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

type SpecState = { findings: Finding[]; unresolved: string[]; breaking: Array<{ id: string; text: string }>; expected: number; version: number; compatibility?: ContractSpec['compatibility'] };
const state = (ev: Evidence) => ev.facts[0].state.facts as unknown as SpecState;

/** What the judge reads beside the spec: the session's artifacts and approvals, the questions answered, its workspace's files, and the
 * directories a ref's file may sit under, the clone first. */
type World = { kinds: Record<string, { versions: SealedArtifact[] }>; approvals: Record<string, Approval>; answered: Set<string>; read: Reader | undefined; roots: string[] };

const escRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// The choices that decide a definition: their pointer is at or under it, or their decision names it; what the person or code asked for shows there.
function decisionsOn(d: Definition, choices: ContractSpec['choices']) {
  const named = d.name && new RegExp(`(?<![\\w$])${escRe(d.name)}(?![\\w$])`);
  const on = (c: ContractSpec['choices'][number]) => (d.pointer && (c.pointer === d.pointer || c.pointer.startsWith(`${d.pointer}/`))) || (named && named.test(c.decision));
  return choices.filter(on).map((c) => ({ choice: c.id, decision: c.decision }));
}

// The definitions that use one: a local $ref to it, or for TypeScript its name in their text; a shared shape is asked for by its users.
function usersOf(d: Definition, defs: Definition[]): string[] {
  const named = typeof d.definition === 'string' && d.name && new RegExp(`(?<![\\w$])${escRe(d.name)}(?![\\w$])`);
  const uses = (o: Definition) => (named ? typeof o.definition === 'string' && named.test(o.definition) : !!d.pointer && JSON.stringify(o.definition).includes(`"#${d.pointer}"`));
  return defs.filter((o) => o !== d && uses(o)).map((o) => o.id);
}

// A contract document splits on its kind's own structure: each top-level definition, with the choices that decide it and the definitions that use it.
function definitionChunks(inputs: Inputs, o: SplitDecl) {
  const [specInput, kindInput] = [o.from].flat();
  const { choices, document } = JSON.parse(inputs[specInput]) as Pick<ContractSpec, 'choices' | 'document'>;
  const defs = kinds[inputs[kindInput] as ContractSpec['kind']].definitions(document);
  return defs.map((d) => ({ id: d.id, key: d.name, text: JSON.stringify({ text: d.definition, decided: decisionsOn(d, choices), usedBy: usersOf(d, defs) }) }));
}
registerSplitter('contract.definitions', definitionChunks);

// The named texts the judge reads: the facts code found, the spec, its choices with the guide rules each cites, and the kind's guide.
async function inputsIn(sealed: SealedArtifact, w: World): Promise<Inputs> {
  const spec = parseSpec(sealed.content) as ContractSpec;
  const kind = kinds[spec.kind];
  // A revision of a failing attempt is a new sealed version but the same spec version; only passed ones can be proposed, so only they count.
  const cleared = (w.kinds[CONTRACT_KIND]?.versions ?? []).slice(0, sealed.version - 1).filter((v) => (v.verdict as { passed?: boolean } | null)?.passed);
  // Breaks are against what callers were given: the approved spec, or before any approval the first that passed.
  const base = approvedSpec(w.kinds, w.approvals, sealed.version) ?? cleared[0];
  const prev = base && (parseSpec(base.content) as ContractSpec);
  const [findings, unresolved, breaking] = await Promise.all([
    kind.check(spec.document),
    unresolvedLinks(spec, guideAnchors(kind.skill), w.answered, w),
    prev ? kind.diff(prev.document, spec.document) : [],
  ]);
  const { intent, choices, document, version, compatibility } = spec;
  const facts = { findings, unresolved, breaking: breaking.map(({ id, text }) => ({ id, text })), expected: cleared.length + 1, version, compatibility };
  const cites = (c: ContractSpec['choices'][number]) => [...new Set(c.why.filter((x) => x.source === 'guide').map((x) => anchorOf(x.ref)))];
  return {
    facts: JSON.stringify(facts),
    spec: JSON.stringify({ intent, choices, document }),
    kind: spec.kind,
    choices: JSON.stringify(choices.map((c) => ({ ...c, cites: cites(c) }))),
    guide: JSON.stringify(Object.fromEntries(guideRules(kind.skill))),
  };
}

// A pitfall asks about the departure itself and its claim holds on "no": a question worded as the claim
// cost Clef its confidence (61% of choices and 74% of definitions answered at 0.8, against 97% and 98% asked this way).
const departure = (instructions: string): Asked => ({ type: 'choice', instructions, options: { yes: 'Yes, this holds.', no: 'No, it does not hold.' } });

// Every choice is judged as a design decision against the whole guide, sent once per request; most cite only answers and code,
// and a departure nobody cited is what the check is for. The document itself is meets_intent's to read.
export const contractDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    spec: { split: { name: 'whole', from: 'spec' }, piece: 'spec' },
    choice: { split: { name: 'jsonItems', from: 'choices' }, piece: 'choice', context: { guide: { name: 'json', from: 'guide' } } },
    definition: { split: { name: 'contract.definitions', from: ['spec', 'kind'] }, piece: 'definition', context: { intent: { name: 'json', from: 'spec', pointer: '/intent' } } },
  },
  // A pitfall costs its weight times the share of items flagged, so one flag among many costs little and fails nothing.
  checks: {
    meets_intent: { scope: 'spec', aggregate: { name: 'all' }, question: { type: 'bool', instructions: 'Does the document deliver the purpose in the intent, and does every guarantee in the intent have a mechanism in the document (a header, a status code, a schema rule)?' } },
    contradicts_guide: { scope: 'choice', aggregate: { name: 'share', pass: ['no'] }, question: departure('Does this choice contradict a guide rule without its why giving a reason from the person or the code? A planned change to current code is not a contradiction.') },
    adds_unasked: { scope: 'definition', aggregate: { name: 'share', pass: ['no'] }, question: departure('Does this definition add an operation, field or guarantee that the intent never asked for, or that one of its non-goals excludes? A definition that serves what the intent asks for (a shared schema, an error shape, a field a guarantee needs), or that a decided choice covers, was asked for.') },
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
  const dirs = async (path: string) => {
    const listed = await env.listDir(path, ctx);
    return listed.ok ? listed.value.filter((f) => f.kind === 'directory' && !f.name.startsWith('.')).map((f) => f.name).sort() : [];
  };
  // The spec is written to artifacts/, so the other directories at the root are clones; theirs (repo/packages) are where a ref may have lost a level.
  const clones = (await dirs('.')).filter((d) => d !== 'artifacts');
  const inner = await Promise.all(clones.map(async (c) => (await dirs(c)).map((d) => `${c}/${d}`)));
  return { read, roots: [...clones, ...inner.flat()] };
}

/** The same inputs from a recording, as the session stood when the spec was sealed: later versions, approvals and answers did not exist yet. */
export async function recordedSpecInputs(session: RecordedSession, content: string): Promise<Inputs> {
  const sealed = session.artifacts[CONTRACT_KIND]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the spec is not a version this session sealed, so what stood when it was judged is unknown');
  if (!session.readFile) throw new Error('the recording keeps no workspace to resolve code refs in');
  const kinds = Object.fromEntries(Object.entries(session.artifacts).map(([k, vs]) => [k, { versions: vs.filter((v) => v.sealedAt <= sealed.sealedAt) }]));
  const approvals = (session.docs['quack.approvals']?.items ?? {}) as unknown as Record<string, Approval>;
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

function linksResolve(_a: string, ev: Evidence): CodeResult {
  const { unresolved } = state(ev);
  return { met: unresolved.length ? 0 : 1, detail: unresolved.length ? `${list(unresolved)}. Cite an answered question by its ask_question id or its text exactly as asked, a guide heading, or an existing path:line.` : undefined };
}

function breakingNamed(_a: string, ev: Evidence): CodeResult {
  const { breaking, compatibility } = state(ev);
  if (!breaking.length) return { met: 1 };
  const named = compatibility?.breaking ?? [];
  const bare = named.filter((b) => !b.callers?.length);
  const ok = named.length > 0 && !bare.length;
  return { met: ok ? 1 : 0, detail: ok ? undefined : `the kind's diff found breaking changes (${list(breaking.map((b) => b.text))}); compatibility.breaking must list each with the callers it breaks, found by reading the code that calls it` };
}

export const contractRubric: Rubric = {
  name: 'contract.spec',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'sound', checks: [
      { id: 'document_clean', label: 'The document breaks a guide rule', weight: 'gate', code: documentSound },
      { id: 'version_matches', label: 'The version number is wrong', weight: 'gate', code: versionMatches },
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
      { id: 'adds_unasked', label: 'The spec adds what nobody asked for', weight: -3 },
    ] },
  ],
};
