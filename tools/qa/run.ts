import { readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';
import { makeCtx, type Scenario, type StepResult } from './scenario.ts';
import { buildWeb, startStack, type StackOpts } from './stack.ts';

const DIR = fileURLToPath(new URL('./scenarios/', import.meta.url));

async function runScenario(s: Scenario, out: string, overrides: StackOpts): Promise<StepResult[]> {
  const results: StepResult[] = [];
  const stack = await startStack({ ...s.stack, ...overrides });
  const browser = await chromium.launch({ executablePath: process.env.QA_CHROMIUM || undefined });
  try {
    await s.run(makeCtx({ url: stack.url, browser }, join(out, s.name), results));
  } finally {
    await browser.close();
    await stack.stop();
  }
  return results;
}

function report(name: string, results: StepResult[]): boolean {
  console.log(`\n${name}`);
  for (const r of results) {
    console.log(`  ${r.ok ? 'PASS' : 'FAIL'}  ${r.name}${r.error ? `: ${r.error}` : ''}`);
    for (const shot of r.shots) console.log(`        ${shot}`);
  }
  return results.every((r) => r.ok);
}

const flag = (name: string) => process.argv[process.argv.indexOf(`--${name}`) + 1];
const has = (name: string) => process.argv.includes(`--${name}`);

async function main(): Promise<void> {
  const out = has('out') ? flag('out') : join(process.cwd(), 'qa-out');
  const only = has('scenario') ? flag('scenario').split(',') : undefined;
  // One build serves every scenario unless --web names an existing dist.
  const overrides: StackOpts = { web: has('web') && flag('web') !== 'current' ? flag('web') : buildWeb() };
  if (has('model')) overrides.model = flag('model') as StackOpts['model'];
  let ok = true;
  for (const file of readdirSync(DIR).filter((f) => f.endsWith('.ts')).sort()) {
    const s = (await import(join(DIR, file))).default as Scenario;
    if (only && !only.includes(s.name)) continue;
    ok = report(s.name, await runScenario(s, out, overrides)) && ok;
  }
  const failed = !ok;
  console.log(failed ? '\nQA FAILED' : '\nQA PASSED');
  process.exit(failed ? 1 : 0);
}

await main();
