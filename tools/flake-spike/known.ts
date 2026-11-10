// The flaky tests already known (#2257, and #2262's sweep), as cases at HEAD, so each detector can be checked against them.
// Usage: node tools/flake-spike/known.ts  (writes results/known-cases.json; then run a detector with `known`)
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Case } from './cases.ts';
import { subjects } from './mutate.ts';
import { ROOT } from './run.ts';

type Known = { id: string; cls: string; file: string; name: string; where: string };
export const KNOWN: Known[] = [
  { id: 'K1', cls: 'port-race', file: 'tools/flake-spike/fixtures/history/h1-qa-stack/qa-stack.test.ts', name: 'the stack serves /version and its stub app, then leaves no listeners', where: '#2257: tools/qa/stack.ts freePort, and the session port at port+1 (pre-fix snapshot)' },
  { id: 'K2', cls: 'readiness-blind', file: 'packages/session/test/retry.test.ts', name: 'a provider abort is retried once and the run completes without a failure', where: 'packages/session/test/retry.test.ts:97: a 3 s poll that gives up silently' },
  { id: 'K3', cls: 'leak', file: 'packages/session/test/ui-routes.test.ts', name: 'errors are RFC 9457 problem+json, and an oversized body is 413', where: 'packages/session/test/ui-routes.test.ts:30: server.close() waits ~6 s on a half-sent upload' },
  { id: 'K4a', cls: 'real-sleep', file: 'packages/distributor/test/distributor.test.ts', name: 'a session process that never reports a port fails with its output and where it was stuck', where: 'packages/distributor/src/process-driver.ts:437/:487: a fixed 5 s report wait' },
  { id: 'K4b', cls: 'real-sleep', file: 'packages/distributor/test/distributor.test.ts', name: 'a session process whose report cannot be written still fails, naming the missing report', where: 'packages/distributor/src/process-driver.ts:437/:487: a fixed 5 s report wait' },
  { id: 'K5', cls: 'real-sleep', file: 'packages/cli/test/cli.test.ts', name: 'show -f prints new transcript lines until the run ends, and returns at once for an idle session', where: 'packages/cli/test/cli.test.ts:106: frames sequenced by 20 and 40 ms timers' },
  { id: 'K6a', cls: 'clock', file: 'packages/sleeper/test/service.test.ts', name: 'a malformed league id escape is a 400', where: 'packages/sleeper/test/service.test.ts:76: reads the wall clock' },
  { id: 'K6b', cls: 'clock', file: 'packages/sleeper/test/service.test.ts', name: 'the page routes are relative to the mount: the league id, and an unknown path is a 404', where: 'packages/sleeper/test/service.test.ts:85: reads the wall clock' },
  { id: 'K7', cls: 'real-sleep', file: 'packages/session/test/exec.test.ts', name: 'a fork bomb hits the process limit: the command fails, the next one runs', where: 'packages/session/test/exec.test.ts:64: ~15 s of bash fork-retry backoff against a 30 s timeout' },
  { id: 'K8', cls: 'shared-state', file: 'tools/qa-stack.test.ts', name: 'the stack serves /version and its stub app, then leaves no listeners', where: 'tools/qa/stack.ts:16: one quack-qa-stack.json in tmpdir shared by every checkout' },
];

export const knownCases = (): Case[] => KNOWN.map((k) => ({
  id: k.id, category: 'flaky', cls: k.cls, file: k.file, name: k.name, runner: 'node', runnable: true, source: k.where,
  subjects: k.id === 'K1' ? ['tools/flake-spike/fixtures/history/h1-qa-stack/stack.ts'] : subjects(k.file),
}));

if (import.meta.main) {
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(join(ROOT, 'tools/flake-spike/results/known-cases.json'), `${JSON.stringify(knownCases(), null, 1)}\n`);
}
