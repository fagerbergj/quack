import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import type { Browser, Locator, Page } from 'playwright';
import type { StackOpts } from './stack.ts';

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
export const SCHEMES = ['light', 'dark'] as const;

/** Runs `fn` in a fresh page for each viewport and colour scheme, so every check covers desktop, phone, light and dark. */
export async function eachView(ctx: Ctx, name: string, fn: (page: Page, label: string) => Promise<void>): Promise<void> {
  for (const viewport of VIEWPORTS) {
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
