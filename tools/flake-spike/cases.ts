// The labeled corpus: each case is one test (a file, or one named test in it), its class, and the code it exercises.
// Classes follow "Software Engineering at Google" (abseil.io/resources/swe-book) chapters 11-13; RULES names the rule each breaks.

export type Category = 'flaky' | 'taut' | 'good';
export type Runner = 'node' | 'vitest' | 'vitest-web';
export type Case = {
  id: string;
  category: Category;
  cls: string;
  /** Repo-relative test file; a `.txt` snapshot is a pre-fix file kept for reading only. */
  file: string;
  /** One test in the file; absent, the whole file is the case. */
  name?: string;
  /** The code under test, mutated by detector D and shown to Clef. */
  subjects: string[];
  runner: Runner;
  /** False for history snapshots whose tree no longer exists. */
  runnable: boolean;
  source: string;
  /** The commit the files are read from; absent, the working tree. */
  rev?: string;
};

export const RULES: Record<string, string> = {
  'port-race': 'ch13 nondeterminism from scheduling; ch11 hermetic: a port reserved then released is shared with every other process',
  'readiness-blind': 'ch11 small tests make no blocking calls; ch13 a wait that cannot see its child die depends on timing',
  'real-sleep': 'ch11 small tests make no sleeps; ch13 sleeps and clock time make outcomes vary with load',
  clock: 'ch13 clock time: the outcome changes with when or how fast it runs',
  'shared-state': 'ch11 hermetic: everything a test needs is set up and torn down by the test itself',
  leak: 'ch11 hermetic teardown: a handle left open outlives the test',
  order: 'ch13 thread scheduling: completion order is not the system under test',
  race: 'ch13 thread scheduling inside the system under test: unchanged code, varying outcome',
  random: 'ch13 nondeterministic input: the outcome can change with the system under test unchanged',
  network: 'ch11/ch13 non-hermetic external services and network latency',
  literal: 'ch11 hollow coverage: lines run, nothing checked',
  truthy: 'ch11 hollow coverage: an assertion any non-empty value passes',
  'mock-self': 'ch13 a low-fidelity double: the test checks the double, not the real path',
  'over-mocked': 'ch13 prefer real implementations, then fakes; mocks last; here the unit itself is replaced',
  'interaction-only': 'ch12 test state, not interactions; ch13 interaction tests rarely find bugs',
  'self-set': 'ch11 hollow coverage: asserts what the test itself just wrote',
  'snapshot-input': 'ch11 hollow coverage: asserts the input, not the output',
  'weak-oracle': 'ch11 hollow coverage: a stub implementation passes the assertion',
  'no-assert': 'ch11 hollow coverage: no assertion runs (none, unreachable, swallowed or unawaited)',
  'logic-in-test': 'ch12 no logic in tests: the expected value is computed, so it shares the bug',
  'change-detector': 'ch13 change-detector: asserts the call sequence, fails on any refactor',
  good: 'ch12 public API, state, behaviors; ch13 real code or fakes; deterministic seams for time and I/O',
};

const S = 'tools/flake-spike/fixtures/subjects/';
const F = 'tools/flake-spike/fixtures/';
const syn = (category: Category, cls: string, id: string, runner: Runner, subjects: string[]): Case => ({
  id: id.slice(0, 3), category, cls, file: `${F}${category}/${id}.test.ts`, subjects: subjects.map((s) => `${S}${s}.ts`), runner, runnable: true, source: 'synthetic',
});
const real = (id: string, file: string, name: string, subjects: string[], runner: Runner = 'node'): Case => ({
  id, category: 'good', cls: 'good', file, name, subjects, runner, runnable: true, source: 'repo',
});

export const CASES: Case[] = [
  { id: 'h1', category: 'flaky', cls: 'port-race', file: `${F}history/h1-qa-stack/qa-stack.test.ts`, subjects: [`${F}history/h1-qa-stack/stack.ts`], runner: 'node', runnable: true, source: '#2257 parent (6d5a8027^): freePort released before the distributor listens; readiness poll blind to its exit' },
  { id: 'h2', category: 'flaky', cls: 'network', file: `${F}history/h2-trends/trends.test.ts.txt`, name: 'the trends workflow keeps reading the service after the session process drops its env vars', subjects: [`${F}history/h2-trends/client.ts.txt`], runner: 'node', runnable: false, source: '#2003 parent: the client bound the real fetch before the test mocked it' },
  { id: 'h3', category: 'flaky', cls: 'race', file: `${F}history/h3-agents/agents.test.ts.txt`, name: 'an agent reads at its current version with its rubric, or one a session pinned', subjects: [`${F}history/h3-agents/process-driver.ts.txt`], runner: 'node', runnable: false, source: '#2118 parent: GET /agents/:name raced the unawaited boot copy (#2117)' },
  { id: 'h4', category: 'flaky', cls: 'port-race', file: `${F}history/h4-distributor/distributor.test.ts.txt`, name: 'routes each session key to its own process', subjects: [`${F}history/h4-distributor/process-driver.ts.txt`], runner: 'node', runnable: false, source: '#1981 parent: freePort released before the session child binds (#1980)' },
  { id: 'h5', category: 'flaky', cls: 'race', file: `${F}history/h5-blast/blast.test.ts.txt`, name: 'checkout_pr reports who calls each changed symbol, from a real language server', subjects: [`${F}history/h5-blast/blast.ts.txt`], runner: 'node', runnable: false, source: '#2003 parent: references query raced the language server project load (10 s guess)' },
  syn('flaky', 'port-race', 'f01-port-race', 'node', ['server']),
  syn('flaky', 'readiness-blind', 'f02-readiness-blind', 'node', ['server', 'echo-child']),
  syn('flaky', 'real-sleep', 'f03-real-sleep', 'node', ['retry']),
  syn('flaky', 'clock', 'f04-elapsed-bound', 'node', ['retry']),
  syn('flaky', 'clock', 'f05-local-day', 'node', ['dates']),
  syn('flaky', 'shared-state', 'f06-shared-tmp', 'node', ['notes']),
  syn('flaky', 'shared-state', 'f07-run-order', 'node', ['config']),
  syn('flaky', 'shared-state', 'f08-shared-module-state', 'vitest', ['notify']),
  syn('flaky', 'leak', 'f09-leaked-interval', 'node', ['heartbeat']),
  syn('flaky', 'leak', 'f10-leaked-server', 'vitest', ['server']),
  syn('flaky', 'random', 'f11-random-jitter', 'node', ['backoff']),
  syn('flaky', 'order', 'f12-completion-order', 'node', ['pool']),
  syn('flaky', 'network', 'f13-network', 'node', ['status']),
  syn('flaky', 'real-sleep', 'f14-vitest-real-timers', 'vitest', ['debounce']),
  syn('flaky', 'clock', 'f15-vitest-wall-clock', 'vitest', ['dates']),
  syn('flaky', 'real-sleep', 'f16-tight-timeout', 'node', ['slug']),
  syn('taut', 'literal', 't01-expect-true', 'vitest', ['slug']),
  syn('taut', 'literal', 't02-assert-ok-true', 'node', ['cart']),
  syn('taut', 'truthy', 't03-truthy-only', 'node', ['slug']),
  syn('taut', 'truthy', 't04-to-be-defined', 'vitest', ['cart']),
  syn('taut', 'mock-self', 't05-mock-self-vitest', 'vitest', ['notify']),
  syn('taut', 'mock-self', 't06-mock-self-node', 'node', ['retry']),
  syn('taut', 'over-mocked', 't07-over-mocked-module', 'vitest', ['cart']),
  syn('taut', 'over-mocked', 't08-over-mocked-method', 'node', ['notify']),
  syn('taut', 'interaction-only', 't09-interaction-called', 'vitest', ['notify']),
  syn('taut', 'interaction-only', 't10-interaction-sleep', 'node', ['retry']),
  syn('taut', 'self-set', 't11-self-set', 'node', ['cart']),
  syn('taut', 'self-set', 't12-self-set-vitest', 'vitest', ['dates']),
  syn('taut', 'snapshot-input', 't13-snapshot-input', 'vitest', ['cart']),
  syn('taut', 'snapshot-input', 't14-snapshot-input-node', 'node', ['slug']),
  syn('taut', 'weak-oracle', 't15-type-only', 'node', ['slug']),
  syn('taut', 'weak-oracle', 't16-weak-bound', 'node', ['cart']),
  syn('taut', 'weak-oracle', 't17-match-anything', 'vitest', ['slug']),
  syn('taut', 'no-assert', 't18-no-assert', 'node', ['cart']),
  syn('taut', 'no-assert', 't19-assert-never-runs', 'node', ['retry']),
  syn('taut', 'no-assert', 't20-swallowed', 'node', ['slug']),
  syn('taut', 'no-assert', 't21-unawaited-expect', 'vitest', ['retry']),
  syn('taut', 'logic-in-test', 't22-expected-from-impl', 'node', ['cart']),
  syn('taut', 'logic-in-test', 't23-logic-in-test', 'node', ['slug']),
  syn('taut', 'change-detector', 't24-change-detector', 'vitest', ['notify']),
  syn('good', 'good', 'g01-retry-fake-sleep', 'node', ['retry']),
  syn('good', 'good', 'g02-echo-port-zero', 'node', ['server']),
  syn('good', 'good', 'g03-notifier-fake', 'node', ['notify']),
  syn('good', 'good', 'g04-debounce-fake-timers', 'vitest', ['debounce']),
  syn('good', 'good', 'g05-cart-table', 'node', ['cart']),
  syn('good', 'good', 'g06-slug-table', 'vitest', ['slug']),
  syn('good', 'good', 'g07-backoff-seeded', 'node', ['backoff']),
  syn('good', 'good', 'g08-notes-own-tmp', 'node', ['notes']),
  syn('good', 'good', 'g09-pool-controlled', 'node', ['pool']),
  real('r01', 'packages/sdk/test/model-retry.test.ts', 'a 429 then an answer decides on the second attempt, after the Retry-After delay', ['packages/sdk/src/model-retry.ts']),
  real('r02', 'packages/sdk/test/model-retry.test.ts', 'an abort during the backoff stops at once, with no further call', ['packages/sdk/src/model-retry.ts']),
  real('r03', 'packages/slots/test/slots.test.ts', 'seven concurrent runs: six hold slots, the seventh waits and gets one when a run ends', ['packages/slots/src/table.ts', 'packages/slots/src/gate.ts']),
  real('r04', 'packages/slots/test/slots.test.ts', 'a crashed holder frees its slot after the lease TTL', ['packages/slots/src/table.ts', 'packages/slots/src/gate.ts']),
  real('r05', 'packages/schedules/test/service.test.ts', 'fires at the occurrence with a deterministic request id, once', ['packages/schedules/src/service.ts']),
  real('r06', 'packages/schedules/test/service.test.ts', 'a cron in America/New_York keeps its wall-clock time across the DST change', ['packages/schedules/src/service.ts']),
  real('r07', 'tools/dupl.test.ts', 'a copy with renamed names in a changed range is reported against the original, once', ['tools/dupl.ts']),
  real('r08', 'tools/leak-check.test.ts', 'a known leak is excused by its repo-relative path, and only that file', ['tools/leak-check.ts']),
  real('r09', 'packages/sdk/test/channel.test.ts', 'a seal whose lifecycle event would pass the cap is refused with a reason the agent can act on', ['packages/sdk/src/channel.ts']),
  real('r10', 'packages/catalog/test/http.test.ts', 'a bad q, pageSize, orderBy or pageToken is a 400 problem', ['packages/catalog/src/http.ts']),
  real('r11', 'packages/judge/test/resolve.test.ts', 'units that share context pack in order up to the cap; one without it, or alone over the cap, goes by itself', ['packages/judge/src/resolve.ts']),
  real('r12', 'packages/judge/test/resolve.test.ts', 'a limiter runs at most n tasks at once and starts the next as one ends', ['packages/judge/src/limit.ts']),
  real('r13', 'packages/github/test/redeliver.test.ts', 'a failed delivery is redelivered once and a succeeded one is not', ['packages/github/src/redeliver.ts']),
  real('r14', 'packages/distributor/test/notifications.test.ts', 'a new pending approval sends one plain-words push to each subscription, and a replay sends none', ['packages/distributor/src/notifications.ts']),
  real('r15', 'packages/session/test/lifecycle.test.ts', 'the stream is the documents at their latest revision, and only a change appends a revision', ['packages/session/src/lifecycle.ts']),
  real('r16', 'packages/research/test/summary.test.ts', 'a report summarizes as its Summary line without markdown or source marks', ['packages/research/src/report.ts']),
  real('r17', 'packages/contracts/test/kind.test.ts', 'validateSpec lists every structure problem, and wants changes and named callers from version 2', ['packages/contracts/src/spec.ts']),
  real('r18', 'tools/qa-stack.test.ts', 'the stack serves /version and its stub app, then leaves no listeners', ['tools/qa/stack.ts']),
  real('r19', 'packages/cli/test/cli.test.ts', 'sessions maps --state and --agent to q, escaping the agent', ['packages/cli/src/main.ts']),
  real('r20', 'packages/web/src/lineDiff.test.ts', 'wordDiff of an empty side is one whole insert or delete, and equal texts have no diff', ['packages/web/src/lineDiff.ts'], 'vitest-web'),
  real('r21', 'packages/web/src/useUpdates.test.tsx', 'a visible page checks every five minutes, and an unchanged build never reloads', ['packages/web/src/useUpdates.ts'], 'vitest-web'),
  real('r22', 'packages/web/src/same.test.ts', 'same compares JSON shapes by value', ['packages/web/src/same.ts'], 'vitest-web'),
];
