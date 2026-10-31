import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { parseReview } from './review.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });

function citesExistingLines(_artifact: string, ev: Evidence): CodeResult {
  const findings = (ev.finding ?? []) as Array<{ id: string; cited?: boolean }>;
  const missing = findings.filter((f) => !f.cited).map((f) => f.id);
  return share(findings.length - missing.length, findings.length, `no existing path:line cited in ${missing.join(', ')}`);
}

// A classifier judges whether a finding sounds right, not whether its quoted code exists; that is a string search.
function quotesExist(_artifact: string, ev: Evidence): CodeResult {
  const findings = (ev.finding ?? []) as Array<{ id: string; misquotes?: string[] }>;
  const wrong = findings.filter((f) => f.misquotes?.length);
  return share(findings.length - wrong.length, findings.length, wrong.map((f) => `${f.id} quotes code that is not in the repository: \`${f.misquotes![0]}\``).join('; '));
}

function blockingNamesAnchor(artifact: string): CodeResult {
  const blocking = parseReview(artifact).findings.filter((f) => f.label === 'blocking');
  const missing = blocking.filter((f) => !f.anchor).map((f) => `finding-${f.index}`);
  return share(blocking.length - missing.length, blocking.length, `no anchor named in ${missing.join(', ')}`);
}

const CI_FAILED = /=(failure|timed_out|cancelled|action_required)\b/;

function verdictMatchesLabels(artifact: string, ev: Evidence): CodeResult {
  const r = parseReview(artifact);
  // Without a blocking finding, an open question or a failing check calls for `comment`. A disclosed gap in verification
  // allows `comment` but does not demand it: "could not run -race here" beside green CI is still an approve.
  const unsettled = r.findings.some((f) => f.label === 'question') || CI_FAILED.test(String(ev.change?.[0]?.state.ci ?? ''));
  const expected = r.findings.some((f) => f.label === 'blocking') ? 'request_changes' : unsettled ? 'comment' : 'approve';
  const gapDisclosed = /could not|unable to|not verified/i.test(r.sections.verified ?? '');
  const ok = r.verdict === expected || (expected === 'approve' && r.verdict === 'comment' && gapDisclosed);
  return { met: ok ? 1 : 0, detail: ok ? undefined : `the findings call for ${expected}, the verdict says ${r.verdict ?? 'nothing'}` };
}

// v0's five code-reviewer criteria, audited into weighted atomic checks (D11, D54). Scopes come from reviewEvidence.
export const reviewerRubric: Rubric = {
  name: 'github.reviewer',
  version: 3,
  threshold: 0.8,
  criteria: [
    { id: 'catches_real_issues', checks: [
      { id: 'no_silent_approval', weight: 'gate', scope: 'change', question: 'If the verdict is approve, is it free of any defect in the diff, or unkept promise from the description or linked issue, that the review never mentions?' },
      // Blocking means it goes wrong once merged; these checks must not demand a blocker for a true issue that does not.
      { id: 'reports_every_defect', weight: 4, scope: 'change', question: 'Is every problem in the diff that goes wrong once this change is merged as it stands (wrong output, a crash, data loss, a security hole) reported as a blocking finding? Problems that only cost time or calls, or only matter if the code changes later, need not be blocking.' },
      { id: 'promises_handled', weight: 3, scope: 'change', question: 'Is every behaviour the description or linked issues say this change delivers either present in the diff, deferred to a linked follow-up, or reported as a blocking delivery finding? A loosely worded sentence about a behaviour the diff does deliver is not an unkept promise.' },
      { id: 'sibling_parts', weight: 2, scope: 'change', question: 'Where the diff adds another instance of an existing pattern, does the review report the parts its siblings carry that this one lacks (or are there none missing)?' },
      { id: 'toolchain_as_defect', weight: -4, scope: 'change', question: 'Does the review report a sandbox, toolchain or network failure as a code defect while the CI checks are green?' },
    ] },
    { id: 'claims_grounded', checks: [
      { id: 'cites_existing_lines', weight: 'gate', code: citesExistingLines },
      { id: 'quotes_exist', weight: 'gate', code: quotesExist },
      { id: 'matches_cited_code', weight: 3, scope: 'finding', question: 'Does the cited code show what this finding says about it?' },
      { id: 'shows_how', weight: 2, scope: 'blocking', question: 'Does this blocking finding show how the problem happens: a failing command and its output, or the input and the line where it goes wrong?' },
      { id: 'verified_names_evidence', weight: 2, scope: 'review', question: 'Does every item under Verified name what was read or run and what it showed, rather than only asserting that tests pass or the logic is correct?' },
      { id: 'contradicted', weight: -4, scope: 'finding', question: 'Does the cited code contradict this finding?' },
    ] },
    { id: 'constructive_actionable', checks: [
      { id: 'says_why', weight: 3, scope: 'finding', question: 'Does this finding say why it matters: the risk, the principle or the benefit, not only that something is wrong?' },
      { id: 'shows_proposed_change', weight: 2, scope: 'finding', question: 'If this finding proposes a specific code change, does it show that code in a fenced block with a language tag (or does it propose no specific change)?' },
      { id: 'answerable', weight: 1, scope: 'finding', question: 'Could the author act on or answer this finding without guessing what is meant?' },
      { id: 'no_reason_given', weight: -2, scope: 'finding', question: 'Does this finding only assert that something is wrong, with no reason at all?' },
    ] },
    { id: 'severity_grounded', checks: [
      { id: 'blocking_names_anchor', weight: 'gate', code: blockingNamesAnchor },
      { id: 'anchor_borne_out', weight: 3, scope: 'blocking', question: 'Does the cited code bear out the anchor this blocking finding names (a defect producing wrong output, missing delivery, a security issue, a named design principle, out-of-scope work, or missing or broken tests)?' },
      { id: 'preference_only', weight: -3, scope: 'blocking', question: 'Does this blocking finding rest only on preference, so the author could decline it at no objective cost?' },
      { id: 'blocks_without_consequence', weight: -4, scope: 'blocking', question: 'Does this blocking finding fail to name something that goes wrong, or a promised behaviour that is missing, once this change is merged as it stands, resting instead on a possible future change, an extra cost, comment or doc wording, or a test that could be stronger?' },
      { id: 'wrong_output_below_blocking', weight: -2, scope: 'lower', question: 'Does this finding, though not labelled blocking, describe wrong output, a crash or data loss?' },
    ] },
    { id: 'structured_verdict', checks: [
      { id: 'verdict_present', weight: 'gate', code: (a) => ({ met: parseReview(a).verdict ? 1 : 0, detail: parseReview(a).verdict ? undefined : 'no verdict' }) },
      { id: 'verdict_matches_labels', weight: 'gate', code: verdictMatchesLabels },
      { id: 'takeaway_actionable', weight: 2, scope: 'review', question: 'Is the takeaway one sentence a reader could act on in fifteen seconds, and does it agree with the findings?' },
    ] },
  ],
};
