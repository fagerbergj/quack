import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import ts from 'typescript';
import type { CodeResult, Evidence, Rubric, Unit } from '@quack/judge';
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

// Why a code ref fails, or undefined when it resolves; refs are read relative to the session workspace, so say where.
async function codeRefProblem(read: Reader | undefined, ref: string): Promise<string | undefined> {
  const m = /^(.+?):(\d+)$/.exec(ref);
  if (!m || !read) return 'is not path:line';
  const text = await read(m[1]);
  if (text === undefined) return `names no file in the workspace (paths are read from the workspace, e.g. repo/${m[1]})`;
  const lines = text.split('\n').length;
  return Number(m[2]) >= 1 && Number(m[2]) <= lines ? undefined : `is past the end of ${m[1]} (${lines} lines)`;
}

async function unresolvedLinks(spec: ContractSpec, anchors: Set<string>, answered: Set<string>, read: Reader | undefined): Promise<string[]> {
  const out: string[] = [];
  for (const c of spec.choices) {
    // A JSON pointer's ~1 escapes read as strikethrough in Markdown, so the note quotes it as code.
    if (!pointerResolves(spec.document, c.pointer)) out.push(`${c.id}: pointer \`${c.pointer}\` is not in the document`);
    for (const w of c.why) {
      const ref = anchorOf(w.ref);
      // An answer's ref is the question as asked; "request" is the person's first description, which is not a question.
      const problem = w.source === 'answer' ? (ref === 'request' || answered.has(w.ref) ? undefined : 'does not resolve') : w.source === 'guide' ? (anchors.has(ref) ? undefined : 'does not resolve') : await codeRefProblem(read, w.ref);
      if (problem) out.push(`${c.id}: ${w.source} ref "${w.ref}" ${problem}`);
    }
  }
  return out;
}

type SpecState = { findings: Finding[]; unresolved: string[]; breaking: Array<{ id: string; text: string }>; expected: number; version: number; compatibility?: ContractSpec['compatibility'] };
const state = (ev: Evidence) => ev.facts[0].state as unknown as SpecState;

/** What the judge reads beside the spec: the session's artifacts and approvals, the questions answered, and its workspace. */
type World = { kinds: Record<string, { versions: SealedArtifact[] }>; approvals: Record<string, Approval>; answered: Set<string>; read: Reader | undefined };

const json = (v: unknown) => JSON.parse(JSON.stringify(v));

// Every choice is judged as a design decision against the whole guide, sent once per request; most cite only answers and code,
// and a departure nobody cited is what the check is for. The document itself is meets_intent's to read.
function choiceUnits(spec: ContractSpec, rules: Map<string, string>): Unit[] {
  const shared = { guide: Object.fromEntries(rules) };
  return spec.choices.map((c) => ({ id: `choice ${c.id}`, shared, state: json({ choice: c, cites: [...new Set(c.why.filter((w) => w.source === 'guide').map((w) => anchorOf(w.ref)))] }) }));
}

const escRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// The choices that decide a definition: their pointer is at or under it, or their decision names it; what the person or code asked for shows there.
function decisionsOn(d: Definition, choices: ContractSpec['choices']) {
  const named = d.name && new RegExp(`(?<![\\w$])${escRe(d.name)}(?![\\w$])`);
  const on = (c: ContractSpec['choices'][number]) => (d.pointer && (c.pointer === d.pointer || c.pointer.startsWith(`${d.pointer}/`))) || (named && named.test(c.decision));
  return choices.filter(on).map((c) => ({ choice: c.id, decision: c.decision }));
}

async function evidenceIn(sealed: SealedArtifact, w: World): Promise<Evidence> {
  const spec = parseSpec(sealed.content) as ContractSpec;
  const kind = kinds[spec.kind];
  // A revision of a failing attempt is a new sealed version but the same spec version; only passed ones can be proposed, so only they count.
  const cleared = (w.kinds[CONTRACT_KIND]?.versions ?? []).slice(0, sealed.version - 1).filter((v) => (v.verdict as { passed?: boolean } | null)?.passed);
  // Breaks are against what callers were given: the approved spec, or before any approval the first that passed.
  const base = approvedSpec(w.kinds, w.approvals, sealed.version) ?? cleared[0];
  const prev = base && (parseSpec(base.content) as ContractSpec);
  const [findings, unresolved, breaking] = await Promise.all([
    kind.check(spec.document),
    unresolvedLinks(spec, guideAnchors(kind.skill), w.answered, w.read),
    prev ? kind.diff(prev.document, spec.document) : [],
  ]);
  const { intent, choices, document, version, compatibility } = spec;
  const facts = { findings, unresolved, breaking: breaking.map(({ id, text }) => ({ id, text })), expected: cleared.length + 1, version, compatibility };
  const defs = kind.definitions(document);
  const shared = json({ intent });
  return {
    facts: [{ id: 'facts', whole: true, state: json(facts) }],
    spec: [{ id: 'spec', whole: true, state: json({ intent, choices, document }) }],
    choice: choiceUnits(spec, guideRules(kind.skill)),
    definition: defs.map((d) => ({ id: `definition ${d.id}`, shared, state: json({ definition: d.id, text: d.definition, decided: decisionsOn(d, choices) }) })),
  };
}

const answeredIn = (items: Record<string, Question>, by = Infinity) => new Set(Object.values(items).filter((q) => q.answer && q.answer.at <= by).map((q) => q.question));

/** What the checks judge: the mechanical findings, the links that do not resolve, the breaking changes since the previous version, and the spec itself with its guide for the judged checks. */
export function specEvidence() {
  return async (api: ToolExecutionApi, sealed: SealedArtifact, ctx: Context): Promise<Evidence> => {
    const read = api.env && (async (path: string) => {
      const r = await api.env!.readTextFile(path, ctx);
      return r.ok ? r.value : undefined;
    });
    return evidenceIn(sealed, {
      kinds: (await api.snapshot(Artifacts, api.conversationId, ctx))?.kinds ?? {},
      approvals: (await api.snapshot(Approvals, api.conversationId, ctx))?.items ?? {},
      answered: answeredIn((await api.snapshot(Questions, api.conversationId, ctx))?.items ?? {}),
      read,
    });
  };
}

/** The same evidence from a recording, as the session stood when the spec was sealed: later versions, approvals and answers did not exist yet. */
export async function recordedSpecEvidence(session: RecordedSession, content: string): Promise<Evidence> {
  const sealed = session.artifacts[CONTRACT_KIND]?.findLast((v) => v.content === content);
  if (!sealed) throw new Error('the spec is not a version this session sealed, so what stood when it was judged is unknown');
  if (!session.readFile) throw new Error('the recording keeps no workspace to resolve code refs in');
  const kinds = Object.fromEntries(Object.entries(session.artifacts).map(([k, vs]) => [k, { versions: vs.filter((v) => v.sealedAt <= sealed.sealedAt) }]));
  const approvals = (session.docs['quack.approvals']?.items ?? {}) as unknown as Record<string, Approval>;
  const questions = (session.docs['quack.questions']?.items ?? {}) as unknown as Record<string, Question>;
  return evidenceIn(sealed, { kinds, approvals, answered: answeredIn(questions, sealed.sealedAt), read: session.readFile });
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
  return { met: unresolved.length ? 0 : 1, detail: unresolved.length ? `${list(unresolved)}. Cite a question id from ask_question, a guide heading, or an existing path:line.` : undefined };
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
  version: 2,
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
      { id: 'meets_intent', label: "The spec doesn't deliver the intent", weight: 4, scope: 'spec', question: 'Does the document deliver the purpose in the intent, and does every guarantee in the intent have a mechanism in the document (a header, a status code, a schema rule)?' },
      { id: 'contradicts_guide', label: 'A choice contradicts the guide', weight: -3, scope: 'choice', question: 'Does this choice contradict a guide rule without its why giving a reason from the person or the code? A planned change to current code is not a contradiction.' },
      { id: 'adds_unasked', label: 'The spec adds what nobody asked for', weight: -3, scope: 'definition', question: 'Does this definition add an operation, field or guarantee that the intent never asked for, or that one of its non-goals excludes? A definition that serves what the intent asks for (a shared schema, an error shape, a field a guarantee needs), or that a decided choice covers, was asked for.' },
    ] },
  ],
};
