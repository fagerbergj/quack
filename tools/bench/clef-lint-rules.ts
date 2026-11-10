// Yes/no lint questions for Clef, one chunk at a time: "yes" is a finding. Each cites the Google guidance it rests on.
// SWE = "Software Engineering at Google" (abseil.io/resources/swe-book); ER = google.github.io/eng-practices/review/reviewer.

export type Lint = 'code' | 'tests' | 'docs' | 'contracts';
export type Rule = { id: string; ask: string; cite: string };

const FOCUS = 'Lines starting with "+" changed in this pull request; judge only those lines, using the rest as context.';

export const RULES: Record<Lint, Rule[]> = {
  tests: [
    { id: 'real-time', ask: 'Does this test code wait on real time: a sleep, a setTimeout or setInterval with a delay, or a loop that polls until a deadline, rather than a fake clock or an awaited event?', cite: 'SWE ch11: small tests make no sleeps or blocking calls; ch13: timing makes outcomes vary with load' },
    { id: 'clock-random', ask: 'Does this test code read the real clock (Date.now(), new Date() with no argument, performance.now()) or Math.random() to build an input or an expected value?', cite: 'SWE ch13: clock time and random input are nondeterminism; the outcome changes with the code unchanged' },
    { id: 'network', ask: 'Does this test code reach a real external service: a public URL, a real model endpoint, or a network host other than localhost?', cite: 'SWE ch11: hermetic tests; external services and network latency cause flakiness' },
    { id: 'shared-state', ask: 'Does this test code depend on state it did not set up itself: a fixed /tmp path, a module-level variable another test changes, or process.env it changes without restoring?', cite: 'SWE ch11: hermetic: everything a test needs is set up and torn down by the test itself' },
    { id: 'interaction-only', ask: 'Does this test assert only on calls made to a mock or spy (call counts, arguments or order) and never on a returned value or resulting state?', cite: 'SWE ch12: test state, not interactions; ch13: change-detector tests fail on any refactor' },
    { id: 'logic', ask: 'Does this test compute its expected value with a loop, a conditional, or the same formula the code under test uses, instead of stating the expected value literally?', cite: 'SWE ch12: no logic in tests; a computed expectation shares the bug' },
    { id: 'hollow', ask: 'Could this test pass with the code under test broken: it asserts a constant or what it just set itself, its assertion can be skipped or never awaited, or it only checks that a value is truthy or defined?', cite: 'SWE ch11: hollow coverage: lines run, nothing checked' },
    { id: 'mocks-subject', ask: 'Does this test replace the unit it is testing with a mock or stub, so the real implementation never runs?', cite: 'SWE ch13: prefer real implementations, then fakes; mocks last' },
    { id: 'behavior-name', ask: 'Is this test named after a method or something generic ("works", "handles input") instead of the behavior and outcome it checks?', cite: 'SWE ch12: test behaviors, not methods; the name states the behavior' },
  ],
  code: [
    { id: 'hard-to-read', ask: 'Would a reader have to study these changed lines at length to know what they do, because intent is hidden, control flow is tangled, or one expression does too much?', cite: 'ER looking-for: "too complex" means it cannot be understood quickly; SWE ch08: optimize for the reader' },
    { id: 'misleading-name', ask: 'Is a name introduced or changed here misleading about what the value holds or what the function does?', cite: 'ER looking-for: Naming: a good name says what it is or does' },
    { id: 'speculative', ask: 'Do these changed lines add generality nothing here needs yet: an option, parameter, hook, or layer of indirection with one or no real use?', cite: 'ER looking-for: no over-engineering; solve the problem known now, not speculative future ones' },
    { id: 'what-comment', ask: 'Does a changed comment narrate what the code does, or describe history or the change itself, instead of explaining why?', cite: 'ER looking-for: Comments explain why code exists, not what it does' },
    { id: 'swallowed-error', ask: 'Do these changed lines swallow a failure: an empty catch, a catch that returns a default with no signal, or a promise whose rejection is ignored?', cite: 'ER looking-for: Functionality: the code does what the author intended for users, including failures' },
    { id: 'edge-case', ask: 'Is there an input these changed lines plainly mishandle: an empty or missing value used unchecked, an off-by-one boundary, or a case the other branches cover but this one forgets?', cite: 'ER looking-for: Functionality: think about edge cases' },
    { id: 'race', ask: 'Could these changed lines race: shared state changed across an await without a guard, an unawaited promise whose result matters, or a check-then-act on state another task changes?', cite: 'ER looking-for: Functionality: concurrency, race conditions and deadlocks' },
    { id: 'health', ask: 'Do these changed lines leave a hack: a TODO, commented-out code, a magic value, or a workaround with no stated reason?', cite: 'ER looking-for / standard: a change should not degrade code health' },
    { id: 'leaky', ask: 'Do these changed lines reach into another module\'s internals, or export an internal only so tests can reach it?', cite: 'ER looking-for: Design: the parts interact sensibly and the change integrates well with the system' },
  ],
  docs: [
    { id: 'mixed-purpose', ask: 'Does this section mix audiences or purposes, such as reference that turns into a tutorial or design rationale, or operator steps mixed with contributor notes?', cite: 'SWE ch10: each document has one audience and one purpose' },
    { id: 'not-current', ask: 'Does this section describe history, plans, decisions or what a change did, instead of how things work now?', cite: 'docguide best practices: delete dead docs; SWE ch10: docs must stay fresh' },
    { id: 'duplicates', ask: 'Does this section restate at length what another document or the code already holds (a full option list, pasted code or command output) instead of linking to it?', cite: 'docguide best practices: link, don\'t duplicate' },
    { id: 'vague', ask: 'Is a step or claim here too vague to act on or check, such as "configure as needed" without saying how?', cite: 'SWE ch10: technical review checks accuracy; developers.google.com/style: be clear and specific' },
    { id: 'unexplained-example', ask: 'Does this section give a command or configuration example without saying what it does or when to use it?', cite: 'SWE ch10: write for the audience; developers.google.com/style highlights' },
    { id: 'padding', ask: 'Is this section padded with preamble, repetition or filler that could be cut without losing information?', cite: 'SWE ch10: keep documents short; docguide: minimum viable documentation' },
  ],
  contracts: [
    { id: 'verb-path', ask: 'Does this API add a path segment that is a verb or action instead of a resource collection or name, without using the resource:verb custom-method form?', cite: 'AIP-121 resource-oriented design; AIP-122 resource names; AIP-136 custom methods' },
    { id: 'method-misuse', ask: 'Does a route here misuse its HTTP method: a GET that changes state or reads a body, a create not on POST, a partial update not on PATCH, or a delete that answers something other than empty or the resource?', cite: 'AIP-131..135 standard methods' },
    { id: 'unpaged-list', ask: 'Does a list route here return a collection that can grow without page size and page token (or cursor) paging?', cite: 'AIP-158 pagination: add paging from the start; adding it later breaks callers' },
    { id: 'adhoc-filter', ask: 'Does a list route here take several one-off filter query parameters instead of one filter expression?', cite: 'AIP-160 filtering: one filter string with a defined grammar' },
    { id: 'breaking', ask: 'Do these changed lines break existing callers: a removed or renamed field or route, a changed type, a new required input or a changed default, with no new major version?', cite: 'AIP-180 backwards compatibility' },
    { id: 'error-shape', ask: 'Does an error response here lack a stable machine-readable reason, or use a status code that does not match the error (500 for bad input, 200 with an error body)?', cite: 'AIP-193 errors: canonical codes plus a machine-readable reason' },
    { id: 'naming', ask: 'Are field or parameter names here inconsistent: mixed case styles, abbreviations, or one concept under two names?', cite: 'AIP-122 / AIP-140 field names: consistent, no abbreviations' },
    { id: 'bare-array', ask: 'Does a response here return a bare JSON array instead of an object with a named field?', cite: 'AIP-132 list responses: a message with a repeated field, so fields can be added' },
  ],
};

/** PR-level question asked once when code changed and no doc did. */
export const DOCS_WITH_CODE: Rule = {
  id: 'docs-with-code',
  ask: 'Does this pull request change something users or operators rely on (CLI commands or flags, configuration keys, an HTTP API, deploy or setup steps) that the repository\'s docs would need to describe?',
  cite: 'docguide best practices: change docs in the same CL as the code',
};

/** Rules checked by code, not Clef. */
export const DETERMINISTIC: Rule[] = [
  { id: 'dangling-ref', ask: 'A path the PR deletes or renames is still named somewhere in the tree at the head.', cite: 'docguide best practices: delete dead docs; links must keep working' },
  { id: 'broken-link', ask: 'A relative link in a changed doc section points at no file at the head.', cite: 'docguide best practices: keep docs fresh' },
  { id: 'logic-without-tests', ask: 'A package\'s src changed by more than 10 lines and none of its tests changed.', cite: 'ER small-cls: tests ship with the logic they cover' },
];

export const instructions = (r: Rule) => `${FOCUS} ${r.ask}`;
