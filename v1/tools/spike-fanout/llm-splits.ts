// Simulated model splits: hand-written stand-ins for the 27B's structured output (the spike may only call Clef), each item with its exact source span.
// The gold lists the report scores against are these same strings, so the simulated split is a ceiling, not a measurement of the 27B.

export type Split = Array<{ item: string; span: string }>;
const each = (spans: string[], say: (s: string) => string = (s) => s): Split => spans.map((span) => ({ item: say(span), span }));

const FIELDS = each([
  'a precise definition',
  'a short code example',
  'how a reviewer can show it from the diff (what to cite)',
  'whether it can be detected mechanically (and how: lint rule, test run, static analysis) or needs judgement',
  'how often it produces false positives',
  'and sources',
], (s) => `For each indicator, give ${s.replace(/^and /, '')}.`);

/** Asks of a research request, keyed by a phrase that identifies the request. */
export const ASKS: Array<{ has: string; items: Split }> = [
  { has: 'budget did Port Alder', items: each(["What budget did Port Alder's council approve for harbour repairs in 2026", 'when does the work start']) },
  { has: 'how much did each cost', items: each(['How many electric buses does Kestrel Transit run as of 2026', 'what is their rated range', 'how much did each cost']) },
  { has: 'electric buses does Kestrel', items: each(['How many electric buses does Kestrel Transit run as of 2026', 'what is their rated range']) },
  { has: 'Marlow Public Library', items: each(['When is Marlow Public Library open on weekends in 2026', 'is parking free']) },
  { has: 'Fernhill Small Farm Grant', items: each(['Who is eligible for the Fernhill Small Farm Grant in 2026', 'what is the maximum award']) },
  {
    has: 'parallel implementations',
    items: [
      ...each(['What are parallel implementations and duplicated logic in a codebase', 'How do reviewers and tools (clone detection such as jscpd or PMD CPD, "rule of three", DRY versus wrong abstraction) detect them', 'when is duplication acceptable', 'Include evidence on AI-generated code duplicating existing utilities.']),
      ...FIELDS,
    ],
  },
  {
    has: 'tautological or vacuous',
    items: [
      ...each(['What makes a test tautological or vacuous, so it passes regardless of whether the code is correct', 'tests that assert their own mocks', 'recompute the expected value with the code under test', 'assert constants', 'have no assertion', 'catch every error', 'snapshot whatever is produced', 'test the framework instead of the code', 'mutation testing', '"fails on the base commit" checks'], (s) => `Does the report cover: ${s}?`),
      ...FIELDS,
    ],
  },
];

/** Promises of a pull request description, by PR number. */
export const PROMISES: Record<number, Split> = {
  2170: each(["the cap counts checks since the run's last pass", "a person's change request after it gets a fresh budget", 'only the newest sealed version can be proposed', 'A run with no pass counts exactly as before']),
  2161: each(['the note is just a field above the options and goes with whichever option you tap', '"Add a note" toggles to "Remove note"', '"Something else…" keeps its Send', 'Below `lg` the composer now hides while an answer field is open', 'empty note → choice alone']),
  2149: each(['the check now says why a code ref fails', 'two clauses say code you cite must be in the workspace', 'contracts goes from v3 to v4, with `version.lock` regenerated', 'Answer and guide refs keep "does not resolve"']),
  2183: each(["the double now serves it with `state: 'open'`"]),
  2179: each(['`judge.test.ts`: a third run with `concurrency: 2` peaks at 2', '`http.test.ts`: a handler that writes and ends after the call was aborted leaves the call rejected with `AbortError` and throws nothing', '`resolve.ts`: the D16 comment moves back above `answerUnit`']),
};

export const askSplitFor = (request: string): Split => ASKS.find((a) => request.includes(a.has))?.items ?? [];
