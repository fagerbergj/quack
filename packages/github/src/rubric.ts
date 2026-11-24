import { PASS_ONLY, registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, factsOf, type Inputs, type Question, type Rubric, type Scope } from '@quack/judge';
import { parseReview, type Citation, type ReviewFacts } from './review.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });

function citesExistingLines(_artifact: string, ev: Evidence): CodeResult {
  const { findings } = factsOf<ReviewFacts>(ev);
  const missing = findings.filter((f) => !f.cited).map((f) => `finding-${f.id}`);
  return share(findings.length - missing.length, findings.length, `no existing path:line cited in ${missing.join(', ')}`);
}

// A check shows its conclusion once completed, its status until then (`name=conclusion ?? status`).
const CI_FAILED = /=(failure|timed_out|cancelled|action_required|stale|startup_failure)\b/;
const CI_PENDING = /=(queued|in_progress|waiting|requested|pending)\b/;
// A negation before a verification verb: "could not run", "unable to reproduce", "not verified", "didn't test".
const GAP = /\b(?:could ?n[o']t|can ?n[o']t|cannot|unable to|did ?n[o']t|was ?n[o']t|were ?n[o']t|not)\s+(?:\w+\s+){0,2}?(?:run|ran|verif\w*|test\w*|check\w*|reproduc\w*|confirm\w*|execut\w*)\b/i;

function verdictMatchesLabels(artifact: string, ev: Evidence): CodeResult {
  const r = parseReview(artifact);
  // Without a blocking finding, an open question or a failing check calls for `comment`. A gap Verified discloses (a check
  // it could not run) allows `comment` but does not demand it: beside green CI it is still an approve.
  const ci = ev.facts ? factsOf<ReviewFacts>(ev).ci : '';
  const unsettled = r.findings.some((f) => f.label === 'question') || CI_FAILED.test(ci);
  const expected = r.findings.some((f) => f.label === 'blocking') ? 'request_changes' : unsettled ? 'comment' : 'approve';
  // CI still running is not green: it allows `comment` the way a disclosed gap does, without demanding it.
  const gapDisclosed = GAP.test(r.sections.verified ?? '') || CI_PENDING.test(ci);
  const ok = r.verdict === expected || (expected === 'approve' && r.verdict === 'comment' && gapDisclosed);
  return { met: ok ? 1 : 0, detail: ok ? undefined : `the findings call for ${expected}, the verdict says ${r.verdict ?? 'nothing'}` };
}

// v0's code-reviewer criteria, kept to checks that caught a real fault in a prod review; reviewDeclaration says how each
// model check is asked. `miss: 4` on matches_cited_code charges any missed citation the -4 that v13 charged only a contradicted one. 0.72 is the lowest threshold that gives no review shape more slack than 0.8 gave before the cut.
export const reviewerRubric: Rubric = {
  name: 'github.reviewer',
  version: 22,
  threshold: 0.72,
  criteria: [
    { id: 'catches_real_issues', checks: [
      { id: 'promises_handled', label: 'A promised behavior is missing', weight: 3 },
    ] },
    { id: 'claims_grounded', checks: [
      { id: 'cites_existing_lines', label: "Cites a line that doesn't exist", weight: 'gate', code: citesExistingLines },
      { id: 'matches_cited_code', label: "Cited code doesn't show the claim", weight: 3, miss: 4 },
      { id: 'verified_names_evidence', label: 'Says verified without saying what was run', weight: 2 },
    ] },
    { id: 'severity_grounded', checks: [
      { id: 'anchor_borne_out', label: "Blocking issue isn't backed by the code", weight: 3 },
      { id: 'blocks_without_consequence', label: 'Blocks with nothing that actually breaks', weight: -4 },
      { id: 'wrong_output_below_blocking', label: 'A serious bug is listed as minor', weight: -2 },
    ] },
    { id: 'structured_verdict', checks: [
      { id: 'verdict_matches_labels', label: 'Verdict disagrees with the findings', weight: 'gate', code: verdictMatchesLabels },
    ] },
  ],
};

const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// Asked as the defect itself, which Clef answers more surely, so "yes" flags it: a pitfall costs its weight times the
// share flagged, and a Verified item that only lists what was read is not mistaken for an unbacked claim.
const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
const SHARE = { name: 'share' } as const;

// Findings of every label see the CI result, the same for all, so a review's findings pack into few decision-model requests.
const findings = (where: 'truthy' | 'falsy'): Scope => ({
  split: { name: 'jsonItems', from: 'findings', pick: ['text', 'citedCode'], where: [{ path: 'blocking', op: where }] },
  piece: 'finding',
  context: { ci: { name: 'json', from: 'facts', pointer: '/ci' } },
});

// A section that reports what was run, measured or left out ("Tests", "QA rig", "Replays", "Not in this PR") promises
// nothing, nor does a sentence that opens that way or whose main clause says a named thing is unchanged; a ### goes with its ##.
const REPORTING = /^\**(?:[\w-]+\s+){0,2}?(?:verif\w*|validat\w*|tests?|testing|checks?|checked|checklist|evidence|commands?|proof|qa|replays?|measured|numbers|rulings?)\b|^\**not\b/i;
const UNCHANGED = /[\w.`-]{2,60}\s+(?:is|are|was|were|stays|remains)\s+(?:\w+\s+){0,2}?(?:unchanged|untouched)\b/i;
const headingOf = (part: string) => /^#{2,3} (.+)/.exec(part)?.[1].trim() ?? 'opening';
const promising = (part: string) => !REPORTING.test(/^#{2,3} (.+)/.exec(part)?.[1] ?? '');
const promiseParts = (inputs: Inputs) => ['description', 'issues'].flatMap((name) => (inputs[name] ?? '').split(/^(?=## )/m)).filter(promising)
  .flatMap((s) => s.split(/^(?=### )/m)).filter(promising);

// A part's sentences outside code blocks, tables, headings and the generated-by footer.
const FENCED = /^(```|~~~)[^\n]*\n[\s\S]*?^\1[ \t]*$/gm;
const sentencesOf = (part: string) => part.replace(FENCED, '').split('\n').map((l) => l.trim())
  .filter((l) => l && !/^[|#]/.test(l) && !/Generated with \[Claude Code\]/.test(l))
  .flatMap((l) => l.replace(/^([-*]|\d+\.)\s+(\[[ x]\]\s*)?/, '').split(/(?<=[.!?])\s+(?=[A-Z`"(*])/));

// What a diff can show: a backticked token without spaces, or a file path (one with a slash, or a source file's name).
const PATH = /(?:[\w.@-]+\/)+[\w@-]+\.[a-z][a-z0-9]{1,4}\b|\b[\w-]+\.(?:ts|tsx|js|mjs|json|md|ya?ml|go|py|sh|sql|css|html)\b/g;
// A path outside the repo (a home or /tmp file, a URL) is no name: no diff shows it.
const namesOf = (s: string) => [...new Set([
  ...[...s.matchAll(/`([^`\s]{2,80})`/g)].map((m) => m[1].replace(/(?:\((?:\.\.\.)?\)|:\d+(?:-\d+)?)$/, '').replace(/^\.\//, '')),
  ...(s.replace(/`[^`]*`|https?:\/\/\S+/g, ' ').match(PATH) ?? []),
])].filter((n) => !/^(?:~|\/tmp\/|https?:)/.test(n));
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// A name is a token: `any` does not match "anyway", and `rubric.ts` matches packages/github/src/rubric.ts.
const tokenOf = (name: string) => new RegExp(`(?<![\\w$])${escape(name)}(?![\\w$])`);

type Hunk = { file: string; text: string };
const hunksOf = (diff: string): Hunk[] => diff.split(/^(?=diff --git )/m).flatMap((f) => {
  const file = /^diff --git a\/\S+ b\/(\S+)/.exec(f)?.[1];
  return file ? f.split(/^(?=@@ )/m).slice(1).map((text) => ({ file, text: `${file}\n${text}` })) : [];
});
// The review's paragraphs and list items, each with its continuation lines.
const blocksOf = (review: string) => review.split(/\n\s*\n|\n(?=\s*(?:[-*]|\d+\.) |#)/).map((b) => `${b.trim()}\n`);

// In JSON characters, which Clef's tokenizer reads at about 3 a token: with the sentence (cut to 1.5k), the missing names
// and the question, an item stays under 8192 tokens.
const HUNK_BUDGET = 15_000;
const REVIEW_BUDGET = 3_000;
const cut = (s: string, max: number) => (s.length > max ? `${s.slice(0, max)}\n… (cut at ${max} characters)` : s);
// As many whole pieces as fit, in order; when the first is too big alone it is cut.
function fit(pieces: string[], budget: number): string {
  let text = '';
  for (const p of pieces) if (JSON.stringify(text + p).length <= budget) text += p;
  return text || (pieces.length ? cut(pieces[0], budget / 2) : '');
}

type Shown = { hunks: Hunk[]; blocks: string[]; files: string; blocking: string };
// The hunks and review text that hold a sentence's names, and which names no hunk holds.
function promiseOf(sentence: string, names: string[], s: Shown) {
  const tokens = names.map(tokenOf);
  const missing = names.filter((_, i) => !s.hunks.some((h) => tokens[i].test(h.text)));
  const shown = fit(s.hunks.filter((h) => tokens.some((t) => t.test(h.text))).map((h) => h.text), HUNK_BUDGET);
  const note = missing.length ? `no hunk contains ${missing.map((x) => `\`${x}\``).join(', ')}; the diff changes ${s.files || 'nothing'}` : '';
  const review = fit(s.blocks.filter((b) => tokens.some((t) => t.test(b))), REVIEW_BUDGET);
  return { sentence: cut(sentence, 1500), diff: [note, shown].filter(Boolean).join('\n\n'), review: review || '(the review names none of these)' };
}

// A finding's text after its `label (anchor): path:line ` prefix, so a name counts as reported only in the finding's prose.
const blockingProse = (findings?: string) => (findings ? (JSON.parse(findings) as Array<{ blocking: boolean; text: string }>) : [])
  .filter((f) => f.blocking).map((f) => f.text.replace(/^[\w-]+(?: \([^)]*\))?: \S+ /, '')).join('\n');

/** One item per promising description sentence that names code or a file, with the hunks and review text holding those names.
 *  A sentence that names nothing makes no item, nor does one whose every name a blocking finding repeats: the review reported it. */
function promiseItems(inputs: Inputs): Chunk[] {
  const hunks = hunksOf(inputs.diff ?? '');
  const s: Shown = { hunks, blocks: blocksOf(inputs.review ?? ''), files: [...new Set(hunks.map((h) => h.file))].join(', ').slice(0, 1000), blocking: blockingProse(inputs.findings) };
  // Numbered over every sentence of both inputs, so two sections sharing a heading still give distinct ids.
  const sentences = promiseParts(inputs).flatMap((part) => sentencesOf(part).map((sentence) => ({ heading: headingOf(part), sentence })));
  return sentences.flatMap(({ heading, sentence }, n) => {
    if (REPORTING.test(sentence) || UNCHANGED.test(sentence.split(/;|, so /)[0])) return [];
    const names = namesOf(sentence);
    if (!names.length || names.every((x) => tokenOf(x).test(s.blocking))) return [];
    return [{ id: `${heading}.s${n + 1}`, text: JSON.stringify(promiseOf(sentence, names, s)) }];
  });
}
registerSplitter('github.promises', promiseItems);

// One item per path:line a finding cites, grouped by finding: the finding goes once on the group's shared context.
// Cases built before the input existed carry none.
const citationItems = (inputs: Inputs): Chunk[] => (inputs.citations ? (JSON.parse(inputs.citations) as Citation[]) : []).map((c, n, all) => ({ id: `${c.group}.${all.slice(0, n + 1).filter((o) => o.group === c.group).length}`, text: JSON.stringify({ cite: c.cite, code: c.code }), meta: { group: c.group, finding: c.finding } }));
registerSplitter('github.citations', citationItems);

// A backticked command line: env assignments, a lowercase executable, then arguments, flags or quoted strings.
const ARG = String.raw`(?:'[^'\`]*'|"[^"\`]*"|-{1,2}[\w-]*(?:=[^\s\`]+)?|[\w./:@*=+-]+)`;
const COMMAND = new RegExp(String.raw`\`(?:[A-Z_]+=[^\s\`]+\s+)*[a-z][a-z0-9._-]*(?:\s+${ARG})+\``, 'g');
// The sources a Verified item can name, found in code so Clef is asked only about items naming none: a command or code it
// read, a file with or without a line, a commit, a URL, CI or the PR's checks, a search.
const SOURCES = [/[\w./-]+\.[A-Za-z]{1,5}:\d+/, /\b(?=[0-9a-f]*[a-f])(?=[0-9a-f]*\d)[0-9a-f]{7,40}\b/, /https?:\/\/\S+/, /\bCI\b/, /`[A-Za-z_$][\w$.]*(?:\(\))?`/,
  /\b[\w-]+(?:\/[\w.-]+)*\.(?:[cm]?[jt]sx?|json|md|go|py|rs|kt|java|ya?ml|toml|sql|sh|css|html)\b/];
// A claim that something is absent (a negation, then a reference, import, use or caller) is backed only by a named search.
const ABSENCE = /\b(?:no|not|nothing|none|nowhere|zero)\b\W+(?:\w+\W+){0,3}?(?:referenc\w*|import\w*|us(?:e|es|ed|age)|call(?:s|ed|ers?)?|remain\w*|left)\b|\b(?:referenc\w*|import\w*|used|called)\s+nowhere\b/i;
const SEARCH_TOOL = /\b(?:grep|rg|ag|ack|find|search)\b/;
const SEARCH_VERB = /\b(?:searched|grepped)\b/i;

/** How many sources a Verified item names, given the names of the PR's CI checks; under a share by `either` an item that names one holds without a model. */
export function namedSources(text: string, checks: string[] = []): number {
  const commands = text.match(COMMAND) ?? [];
  if (ABSENCE.test(text) && !SEARCH_VERB.test(text) && !commands.some((c) => SEARCH_TOOL.test(c))) return 0;
  // A check is named in backticks or as name=conclusion: a bare word like "check" is not a citation of the job.
  const ci = checks.some((name) => new RegExp(String.raw`\`${escape(name)}\`|\b${escape(name)}=\w+`).test(text));
  return SOURCES.filter((rx) => rx.test(text)).length + (commands.length ? 1 : 0) + (ci ? 1 : 0) + (SEARCH_VERB.test(text) ? 1 : 0);
}
// The PR's check names come from its CI facts ("name=conclusion, ..."). The value stays empty so an item Clef is asked
// about reads as it did before the pre-check.
const checkNames = (inputs: Inputs) => (inputs.facts ? (JSON.parse(inputs.facts) as ReviewFacts).ci.split(', ').map((c) => c.split('=')[0]).filter(Boolean) : []);
registerSelector('github.namedSources', (inputs, chunk) => ({ value: '', matches: namedSources(chunk.text, checkNames(inputs)) }));

/** How each model check of reviewerRubric is asked: per finding with its cited code, per PR description sentence with the hunks it names, or per Verified item. */
export const reviewDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    verified: { split: { name: 'listItems', from: 'review', sections: ['Verified'] }, piece: 'item', context: { sources: { name: 'github.namedSources' } } },
    blocking: findings('truthy'),
    nonblocking: findings('falsy'),
    citation: { split: { name: 'github.citations', from: 'citations' }, piece: 'excerpt' },
    // promises_handled is pass-only: a sentence whose names no hunk contains is the rare escalation the judge model explains.
    // github.promises reads these five inputs by name; only the description and issues split into sentences.
    promise: { split: { name: 'github.promises', from: ['description', 'issues', 'diff', 'findings', 'review'] }, piece: 'promise' },
  },
  checks: {
    // Blocking means it goes wrong once merged; these checks must not demand a blocker for a true issue that does not.
    // Promises and Verified items earn the share that holds: under `all`, one noisy answer among many items flips the check.
    promises_handled: { scope: 'promise', aggregate: SHARE, decisionModel: PASS_ONLY, question: yes('Is the behaviour this sentence of the pull request description says the change has either present in the `diff`, deferred to a linked follow-up, reported in the `review` as a blocking finding, or intentionally not delivered with the `review` explaining why the promise was walked back? A sentence that describes the problem or the state before this change, rather than what the change does, holds.') },
    // A finding holds when every line it cites shows what it says there; a contradiction is the plainest way it does not.
    matches_cited_code: { scope: 'citation', aggregate: { name: 'grouped', groupBy: 'group', inner: 'all', outer: 'share' }, question: yes('Does this excerpt show what the finding says about the code at its `cite`, without contradicting the finding?') },
    // An item that names a source holds in code, so Clef is asked only of items that name none.
    verified_names_evidence: { scope: 'verified', aggregate: { name: 'share', by: 'either' }, question: defect('Does this item under Verified claim a result (tests pass, the logic is correct, nothing references something) without naming what was read or run to show it?') },
    anchor_borne_out: { scope: 'blocking', aggregate: SHARE, question: yes('Does the cited code bear out the anchor this blocking finding names (a defect producing wrong output, missing delivery, a security issue, a named design principle, out-of-scope work, or missing or broken tests)?') },
    blocks_without_consequence: { scope: 'blocking', aggregate: SHARE, question: yes("Would merging this change as it stands cause the harm this blocking finding's anchor names: wrong output, a crash or data loss (defect), promised behaviour missing (delivery), a security hole, a named design principle broken (design), work outside the change's purpose (scope), or a missing or broken test (tests)? Answer no when the finding asks for a nit, an edit to the pull request description, a doc or a comment, a follow-up, or tooling polish.") },
    wrong_output_below_blocking: { scope: 'nonblocking', aggregate: SHARE, question: defect('Does this finding, though not labelled blocking, describe wrong output, a crash or data loss?') },
  },
};
