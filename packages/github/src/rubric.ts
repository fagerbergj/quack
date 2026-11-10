import { registerSelector, registerSplitter, type Chunk, type CodeResult, type Declaration, type Evidence, type Inputs, type Question, type Rubric, type Scope, type SplitDecl } from '@quack/judge';
import { parseReview, type ReviewFacts } from './review.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const factsOf = (ev: Evidence) => ev.facts[0].state.facts as unknown as ReviewFacts;

function citesExistingLines(_artifact: string, ev: Evidence): CodeResult {
  const { findings } = factsOf(ev);
  const missing = findings.filter((f) => !f.cited).map((f) => `finding-${f.id}`);
  return share(findings.length - missing.length, findings.length, `no existing path:line cited in ${missing.join(', ')}`);
}

function blockingNamesAnchor(artifact: string): CodeResult {
  const blocking = parseReview(artifact).findings.filter((f) => f.label === 'blocking');
  const missing = blocking.filter((f) => !f.anchor).map((f) => `finding-${f.index}`);
  return share(blocking.length - missing.length, blocking.length, `no anchor named in ${missing.join(', ')}`);
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
  const ci = ev.facts ? factsOf(ev).ci : '';
  const unsettled = r.findings.some((f) => f.label === 'question') || CI_FAILED.test(ci);
  const expected = r.findings.some((f) => f.label === 'blocking') ? 'request_changes' : unsettled ? 'comment' : 'approve';
  // CI still running is not green: it allows `comment` the way a disclosed gap does, without demanding it.
  const gapDisclosed = GAP.test(r.sections.verified ?? '') || CI_PENDING.test(ci);
  const ok = r.verdict === expected || (expected === 'approve' && r.verdict === 'comment' && gapDisclosed);
  return { met: ok ? 1 : 0, detail: ok ? undefined : `the findings call for ${expected}, the verdict says ${r.verdict ?? 'nothing'}` };
}

// v0's five code-reviewer criteria, audited into weighted atomic checks; reviewDeclaration says how each model check is asked.
export const reviewerRubric: Rubric = {
  name: 'github.reviewer',
  version: 8,
  threshold: 0.8,
  criteria: [
    { id: 'catches_real_issues', checks: [
      { id: 'approval_grounded', label: 'Approves without saying why it is safe', weight: 3 },
      { id: 'promises_handled', label: 'A promised behavior is missing', weight: 3 },
      { id: 'toolchain_as_defect', label: 'Blames code for a tooling failure', weight: -4 },
    ] },
    { id: 'claims_grounded', checks: [
      { id: 'cites_existing_lines', label: "Cites a line that doesn't exist", weight: 'gate', code: citesExistingLines },
      { id: 'matches_cited_code', label: "Cited code doesn't show the claim", weight: 3 },
      { id: 'shows_how', label: "Blocking issue doesn't show how it fails", weight: 2 },
      { id: 'verified_names_evidence', label: 'Says verified without saying what was run', weight: 2 },
      { id: 'contradicted', label: 'Cited code contradicts the finding', weight: -4 },
    ] },
    { id: 'constructive_actionable', checks: [
      { id: 'says_why', label: "Doesn't say why a finding matters", weight: 3 },
      { id: 'shows_proposed_change', label: 'Proposes a change without showing the code', weight: 2 },
      { id: 'answerable', label: 'Finding is too vague to act on', weight: 1 },
      { id: 'no_reason_given', label: "Says it's wrong with no reason", weight: -2 },
    ] },
    { id: 'severity_grounded', checks: [
      { id: 'blocking_names_anchor', label: 'Blocking issue names no concrete cause', weight: 'gate', code: blockingNamesAnchor },
      { id: 'anchor_borne_out', label: "Blocking issue isn't backed by the code", weight: 3 },
      { id: 'preference_only', label: 'Blocks on mere preference', weight: -3 },
      { id: 'blocks_without_consequence', label: 'Blocks with nothing that actually breaks', weight: -4 },
      { id: 'wrong_output_below_blocking', label: 'A serious bug is listed as minor', weight: -2 },
    ] },
    { id: 'structured_verdict', checks: [
      { id: 'verdict_present', label: 'Review has no verdict', weight: 'gate', code: (a) => ({ met: parseReview(a).verdict ? 1 : 0, detail: parseReview(a).verdict ? undefined : 'no verdict' }) },
      { id: 'verdict_matches_labels', label: 'Verdict disagrees with the findings', weight: 'gate', code: verdictMatchesLabels },
      { id: 'takeaway_actionable', label: "Takeaway isn't a quick, clear action", weight: 2 },
    ] },
  ],
};

const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// Asked as the defect itself, which Clef answers more surely, so "yes" flags it: a pitfall costs its weight times the
// share flagged, and a Verified item that only lists what was read is not mistaken for an unbacked claim.
const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
const ALL = { name: 'all' } as const;
const SHARE = { name: 'share' } as const;

// Findings of every label see the CI result, the same for all, so a review's findings pack into few decision-model requests.
const findings = (where?: 'truthy' | 'falsy'): Scope => ({
  split: { name: 'jsonItems', from: 'findings', pick: ['text', 'citedCode'], ...(where ? { where: [{ path: 'blocking', op: where }] } : {}) },
  piece: 'finding',
  context: { ci: { name: 'json', from: 'facts', pointer: '/ci' } },
});

// A PR description splits at its own ## and ### headings, its opening first: each part makes its own promises. A part whose
// heading is a short label for verification ("Tests", "Smoke test (…)", "Evidence") lists what was run and promises nothing.
const VERIFYING = /^(?:[\w-]+\s+){0,2}?(?:verif\w*|validat\w*|tests?|testing|checks?|checked|checklist|evidence|commands?|proof|qa)\s*(?:\(.*\))?$/i;
function descriptionParts(inputs: Inputs, o: SplitDecl): Chunk[] {
  const parts = [o.from].flat().flatMap((name) => inputs[name].split(/^(?=#{2,3} )/m));
  return parts.map((text) => ({ id: /^#{2,3} (.+)/.exec(text)?.[1].trim() ?? 'opening', text: text.trim() })).filter((c) => c.text && !(c.id !== 'opening' && VERIFYING.test(c.id)));
}
registerSplitter('github.descriptionParts', descriptionParts);

// A backticked command line: env assignments, a lowercase executable, then arguments, flags or quoted strings.
const ARG = String.raw`(?:'[^'\`]*'|"[^"\`]*"|-{1,2}[\w-]*(?:=[^\s\`]+)?|[\w./:@*=+-]+)`;
const COMMAND = new RegExp(String.raw`\`(?:[A-Z_]+=[^\s\`]+\s+)*[a-z][a-z0-9._-]*(?:\s+${ARG})+\``, 'g');
// The sources a Verified item can name, found in code so Clef is asked only about items naming none: a backticked
// command, a file:line, a commit sha, a URL, CI (by that word, or one of the PR's own checks), or a search it ran.
const SOURCES = [/[\w./-]+\.[A-Za-z]{1,5}:\d+/, /\b(?=[0-9a-f]*[a-f])(?=[0-9a-f]*\d)[0-9a-f]{7,40}\b/, /https?:\/\/\S+/, /\bCI\b/];
// A claim that something is absent (a negation, then a reference, import, use or caller) is backed only by a named search.
const ABSENCE = /\b(?:no|not|nothing|none|nowhere|zero)\b\W+(?:\w+\W+){0,3}?(?:referenc\w*|import\w*|us(?:e|es|ed|age)|call(?:s|ed|ers?)?|remain\w*|left)\b|\b(?:referenc\w*|import\w*|used|called)\s+nowhere\b/i;
const SEARCH_TOOL = /\b(?:grep|rg|ag|ack|find|search)\b/;
const SEARCH_VERB = /\b(?:searched|grepped)\b/i;
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

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

/** How each model check of reviewerRubric is asked: per finding with its cited code, per part of the PR description with the diff, per Verified item, or of the review whole. */
export const reviewDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    review: { split: { name: 'whole', from: 'review' }, piece: 'review' },
    approve: { split: { name: 'whole', from: 'approve' }, piece: 'review' },
    verified: { split: { name: 'listItems', from: 'review', sections: ['Verified'] }, piece: 'item', context: { sources: { name: 'github.namedSources' } } },
    finding: findings(),
    blocking: findings('truthy'),
    nonblocking: findings('falsy'),
    // Each part sees the whole diff (cut at 120k characters; retrieved hunks missed delivering helpers). With it a part rarely
    // fits Clef, so promises skip Clef and go to the judge model, all of one review's parts in one request.
    promise: {
      split: { name: 'github.descriptionParts', from: ['description', 'issues'] },
      piece: 'part',
      context: {
        diff: { name: 'whole', from: 'diff', maxChars: 120_000 },
        review: { name: 'whole', from: 'review' },
      },
    },
  },
  checks: {
    approval_grounded: { scope: 'approve', aggregate: ALL, question: yes('This review approves. Do its Takeaway and Verified sections say what about the change makes it safe to merge (which behaviour was checked and how it was checked), rather than only praising the change or asserting that it looks good or that tests pass?') },
    // Blocking means it goes wrong once merged; these checks must not demand a blocker for a true issue that does not.
    // Promises and Verified items earn the share that holds: under `all`, one noisy answer among many items flips the check.
    promises_handled: { scope: 'promise', aggregate: SHARE, classifier: false, question: yes('Is every behaviour this part of the pull request description says the change delivers either present in the diff, deferred to a linked follow-up, or reported in the review as a blocking finding? A part that promises no behaviour holds, and a loosely worded sentence about a behaviour the diff does deliver is not an unkept promise.') },
    toolchain_as_defect: { scope: 'finding', aggregate: SHARE, question: defect('Does this finding report a sandbox, toolchain or network failure as a code defect while the CI checks are green?') },
    matches_cited_code: { scope: 'finding', aggregate: SHARE, question: yes('Does the cited code show what this finding says about it?') },
    shows_how: { scope: 'blocking', aggregate: SHARE, question: yes('Does this blocking finding show how the problem happens: a failing command and its output, or the input and the line where it goes wrong?') },
    // Clef reads a loosely named source (a file without a line, a described search) as none, so the judge model answers.
    verified_names_evidence: { scope: 'verified', aggregate: { name: 'share', by: 'either' }, classifier: false, question: defect('Does this item under Verified claim a result (tests pass, the logic is correct, nothing references something) without naming what was read or run to show it?') },
    // Clef answers this without tracing the cited code, so it goes to the judge model.
    contradicted: { scope: 'finding', aggregate: SHARE, classifier: false, question: defect('Does the cited code contradict this finding?') },
    // Clef answers this without weighing the consequence a finding states, so it goes to the judge model.
    says_why: { scope: 'finding', aggregate: SHARE, classifier: false, question: yes('Does this finding say why it matters: the risk, the principle or the benefit, not only that something is wrong?') },
    shows_proposed_change: { scope: 'finding', aggregate: SHARE, question: yes('If this finding proposes a specific code change, does it show that code in a fenced block with a language tag (or does it propose no specific change)?') },
    answerable: { scope: 'finding', aggregate: SHARE, question: yes('Could the author act on or answer this finding without guessing what is meant?') },
    no_reason_given: { scope: 'finding', aggregate: SHARE, question: defect('Does this finding only assert that something is wrong, with no reason at all?') },
    anchor_borne_out: { scope: 'blocking', aggregate: SHARE, question: yes('Does the cited code bear out the anchor this blocking finding names (a defect producing wrong output, missing delivery, a security issue, a named design principle, out-of-scope work, or missing or broken tests)?') },
    preference_only: { scope: 'blocking', aggregate: SHARE, question: defect('Does this blocking finding rest only on preference, so the author could decline it at no objective cost?') },
    blocks_without_consequence: { scope: 'blocking', aggregate: SHARE, question: defect('Does this blocking finding fail to name something that goes wrong, or a promised behaviour that is missing, once this change is merged as it stands, resting instead on a possible future change, an extra cost, comment or doc wording, or a test that could be stronger?') },
    wrong_output_below_blocking: { scope: 'nonblocking', aggregate: SHARE, question: defect('Does this finding, though not labelled blocking, describe wrong output, a crash or data loss?') },
    takeaway_actionable: { scope: 'review', aggregate: ALL, question: yes('Is the takeaway one sentence a reader could act on in fifteen seconds, and does it agree with the findings?') },
  },
};
