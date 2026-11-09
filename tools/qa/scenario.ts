import { createHmac } from 'node:crypto';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import type { Browser, Locator, Page } from 'playwright';
import { QA_WEBHOOK_SECRET, type StackOpts } from './stack.ts';

let delivery = 0;

/** Signs and sends a pull_request webhook as GitHub would, so the github app (`--apps github`) writes the PR to its mirror and streams it. */
export async function prWebhook(url: string, fullName: string, number: number, state: 'open' | 'merged' | 'closed') {
  const at = state === 'open' ? null : '2026-11-09T12:00:00Z';
  const pull_request = { number, state: state === 'open' ? 'open' : 'closed', merged: state === 'merged', merged_at: state === 'merged' ? at : null, closed_at: at, html_url: `https://github.com/${fullName}/pull/${number}`, title: `PR ${number}` };
  const [login, name] = fullName.split('/');
  const body = JSON.stringify({ action: state === 'open' ? 'reopened' : 'closed', repository: { full_name: fullName, name, owner: { login } }, pull_request });
  const signature = `sha256=${createHmac('sha256', QA_WEBHOOK_SECRET).update(body).digest('hex')}`;
  const res = await fetch(`${url}/apps/github/webhook`, { method: 'POST', body, headers: { 'x-github-event': 'pull_request', 'x-hub-signature-256': signature, 'x-github-delivery': `qa-${process.pid}-${++delivery}` } });
  if (!res.ok) throw new Error(`webhook ${state} ${fullName}#${number}: ${res.status} ${await res.text()}`);
}

/** Starts a quack session under a github review key through the stack's fake model, so the list and header have a review to show. */
export async function seedReview(url: string, key: string) {
  const res = await fetch(`${url}/sessions/${encodeURIComponent(key)}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: `Review PR ${key.split('#')[1]}`, wait: true }) });
  if (!res.ok) throw new Error(`seed ${key}: ${res.status} ${await res.text()}`);
}

export type StepResult = { name: string; ok: boolean; error?: string; shots: string[] };
export type Ctx = {
  url: string;
  browser: Browser;
  /** Runs `fn` as one named step; a throw fails the step and the scenario goes on to the next. */
  step(name: string, fn: () => Promise<void>): Promise<void>;
  /** Saves a full-viewport screenshot named after the step and returns its path. */
  shot(page: Page, name: string): Promise<string>;
};
/** What a scenario file default-exports: the stack it needs and the steps to run against it. */
export type Scenario = { name: string; stack: StackOpts; run(ctx: Ctx): Promise<void> };

/** Asserts the locator shows within 10 s; throws otherwise, which fails the step. */
export const visible = (locator: Locator) => locator.waitFor({ state: 'visible', timeout: 10_000 });

export const VIEWPORTS = [{ width: 1280, height: 800 }, { width: 400, height: 800 }];
/** VIEWPORTS plus a 1920 px desktop, for layouts that change with the room beside the chat. */
export const WITH_WIDE = [...VIEWPORTS, { width: 1920, height: 1080 }];
export const SCHEMES = ['light', 'dark'] as const;

/** Runs `fn` in a fresh page for each viewport and colour scheme, so every check covers desktop, phone, light and dark. */
export async function eachView(ctx: Ctx, name: string, fn: (page: Page, label: string) => Promise<void>, viewports = VIEWPORTS): Promise<void> {
  for (const viewport of viewports) {
    for (const colorScheme of SCHEMES) {
      const label = `${viewport.width}-${colorScheme}`;
      await ctx.step(`${name} @${label}`, async () => {
        const context = await ctx.browser.newContext({ viewport, colorScheme });
        try {
          const page = await context.newPage();
          await fn(page, label);
        } finally {
          await context.close();
        }
      });
    }
  }
}

export function makeCtx(base: Omit<Ctx, 'step' | 'shot'>, outDir: string, results: StepResult[]): Ctx {
  mkdirSync(outDir, { recursive: true });
  let current: StepResult | undefined;
  return {
    ...base,
    async step(name, fn) {
      current = { name, ok: true, shots: [] };
      results.push(current);
      try {
        await fn();
      } catch (err) {
        current.ok = false;
        current.error = (err as Error).message.split('\n')[0];
      }
    },
    async shot(page, name) {
      const path = join(outDir, `${name.replace(/[^\w.-]+/g, '_')}.png`);
      await page.screenshot({ path });
      current?.shots.push(path);
      return path;
    },
  };
}
