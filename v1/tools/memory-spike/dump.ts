// Usage: node tools/memory-spike/dump.ts <session-dir> [--full]
// Prints one copied prod session as text: operator inputs, tool calls, result heads and the agent's messages.
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';

type Part = { type: string; text?: string; thinking?: string; name?: string; arguments?: unknown };
const [dir, flag] = process.argv.slice(2);
const full = flag === '--full';
const cut = (s: string, n: number) => (full || s.length <= n ? s : `${s.slice(0, n)} …[${s.length - n} more]`);
const parts = (c: unknown): Part[] => (typeof c === 'string' ? [{ type: 'text', text: c }] : (c as Part[]) ?? []);

const store = await openNodeSqliteStorage(join(dir, 'session.sqlite'));
console.log(`# ${(await readFile(join(dir, 'session-key'), 'utf8')).trim()}  created ${new Date(Number(await readFile(join(dir, 'created'), 'utf8'))).toISOString()}`);
let cursor: unknown;
do {
  const page = await store.scanEntries({ conversationId: 1 as never, order: 'ascending' }, 200, cursor as never, ctx);
  for (const e of page.items) {
    const m = e.model?.[0] as { role?: string; content?: unknown; toolName?: string } | undefined;
    if (e.kind === 'pi.system') continue;
    for (const p of parts(m?.content)) {
      if (e.kind === 'pi.user' && p.text) console.log(`\n[${e.id} OPERATOR] ${cut(p.text, 3000)}`);
      else if (e.kind === 'pi.tool-result' && p.text) console.log(`[${e.id} result ${m?.toolName}] ${cut(p.text, 400)}`);
      else if (p.type === 'toolCall') console.log(`[${e.id} call ${p.name}] ${cut(JSON.stringify(p.arguments), 400)}`);
      else if (p.type === 'text' && p.text?.trim()) console.log(`[${e.id} AGENT] ${cut(p.text.trim(), 1500)}`);
    }
  }
  cursor = page.next;
} while (cursor);
await store.close(ctx);
