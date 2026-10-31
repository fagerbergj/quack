import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { Type, type TSchema } from '@earendil-works/pi-ai';
import type { JsonValue } from '@earendil-works/chord';
import { defineExtension, defineTool, type Extension, type ToolExecutionResult } from '@earendil-works/pi-durable';
import { createJiti } from 'jiti';

type PiResult = { content: Array<{ type: string; text?: string }>; details?: unknown; isError?: boolean };
type PiTool = {
  name: string;
  description: string;
  parameters: TSchema;
  execute(callId: string, params: unknown, signal: AbortSignal | undefined, onUpdate: unknown, ctx: unknown): Promise<PiResult>;
};
type PiFactory = (pi: unknown) => void | Promise<void>;

// Pi coding-agent sessions show UI; a quack session has none, so every UI call answers "nothing chosen" (A16).
const noUi = new Proxy({}, { get: () => async () => undefined });

const shutdownHandlers: Array<() => unknown> = [];

/** Runs the session_shutdown handlers loaded packages registered, such as pi-lsp stopping its language servers. */
export async function piShutdown(): Promise<void> {
  await Promise.allSettled(shutdownHandlers.splice(0).map((h) => h()));
}

/** The pi ExtensionAPI subset tool packages use: registerTool and session_shutdown are real; anything else is a no-op. */
function fakeApi(tools: PiTool[]) {
  const on = (event: string, handler: () => unknown) => { if (event === 'session_shutdown') shutdownHandlers.push(handler); };
  const api = { registerTool: (t: PiTool) => void tools.push(t), on, ui: noUi, events: { on: () => {}, emit: () => {} } };
  return new Proxy(api, { get: (target, key) => (key in target ? target[key as keyof typeof target] : () => {}) });
}

function parsed(text: string): JsonValue {
  try {
    return JSON.parse(text) as JsonValue;
  } catch {
    return text;
  }
}

function adapt(tool: PiTool, replaySafe: boolean) {
  return defineTool({
    name: tool.name,
    description: tool.description,
    parameters: tool.parameters,
    replay: replaySafe ? 'safe' : undefined,
    // Programs calling the tool get its whole text, parsed when it is JSON, rather than the model's bounded copy.
    structuredOutputSchema: Type.Unknown(),
    async execute(args, api, ctx): Promise<ToolExecutionResult> {
      const piCtx = { cwd: api.env?.cwd ?? process.cwd(), signal: ctx.abortSignal, hasUI: false, ui: noUi };
      const r = await tool.execute(api.callId, args, ctx.abortSignal, undefined, piCtx);
      const output = r.content.filter((c) => c.type === 'text').map((c) => ({ type: 'text' as const, text: c.text ?? '' }));
      return { output, isError: r.isError, structuredOutput: parsed(output.map((o) => o.text).join('\n')) };
    },
  });
}

/** The entry points a pi package declares in its package.json `pi.extensions`. */
function entries(pkg: string, from: string): string[] {
  const require = createRequire(from);
  let manifest: string;
  try {
    manifest = require.resolve(`${pkg}/package.json`);
  } catch (err) {
    throw new Error(`pi package ${pkg}: ${(err as Error).message}`);
  }
  const declared = (JSON.parse(readFileSync(manifest, 'utf8')).pi?.extensions ?? []) as string[];
  if (!declared.length) throw new Error(`${pkg} declares no pi.extensions`);
  return declared.map((e) => join(dirname(manifest), e));
}

// Loads a pi package's tools as a pi-durable extension, through jiti as pi does (A16). Only registerTool carries
// over; tool_call and tool_result hooks and all UI do not.
export async function piPackage(pkg: string, opts: { replaySafe?: string[]; /** Resolve the package from here; pass the caller's import.meta.url. */ from?: string } = {}): Promise<Extension> {
  const jiti = createJiti(import.meta.url);
  const tools: PiTool[] = [];
  for (const entry of entries(pkg, opts.from ?? import.meta.url)) {
    const mod = (await jiti.import(entry)) as { default?: PiFactory };
    if (typeof mod.default !== 'function') throw new Error(`${entry} has no default extension factory`);
    await mod.default(fakeApi(tools));
  }
  const safe = new Set(opts.replaySafe ?? []);
  return defineExtension({ name: `pi:${pkg}`, tools: tools.map((t) => adapt(t, safe.has(t.name))) });
}
