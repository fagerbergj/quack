import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';

export type Run = { command: string; failed: boolean };

const PR = 'artifacts/pr.md';
type Tx = Parameters<Parameters<ToolExecutionApi['commit']>[0]>[0];

// shortcut: a backticked span with a space counts as a command and matching is by normalized substring, so prose like `a = 3` can false-flag and a command run inside a script is missed; upgrade by having the agent list its runs in a structured block.
const norm = (s: string) => s.replace(/['"]/g, '').replace(/(^|&&|;)\s*cd\s+\S+\s*&&/g, '$1').replace(/\s+/g, ' ').trim();

const spans = (text: string) => [...text.matchAll(/`([^`\n]+)`/g)].map((m) => ({ code: m[1].trim(), line: text.slice(text.lastIndexOf('\n', m.index) + 1, text.indexOf('\n', m.index)) }));

/** Backticked commands in the "How verified" part of the PR text, with the line that claims them. */
export function claimedCommands(text: string) {
  const at = text.search(/^\W*(how )?verified\b/im);
  if (at < 0) return [];
  const section = text.slice(at).split(/\n#{1,6} /)[0];
  return spans(section).filter((s) => /^[\w./-]+ \S/.test(s.code));
}

/** Short hex ids in backticks anywhere in the PR text, which name commits. */
export const claimedShas = (text: string) => spans(text).map((s) => s.code).filter((c) => /^(?=.*\d)[0-9a-f]{7,40}$/.test(c));

/** Every bash call in this session with whether it exited non-zero, read from the transcript. */
export async function sessionRuns(api: ToolExecutionApi, ctx: Context): Promise<Run[]> {
  const commands = new Map<string, string>();
  const runs: Run[] = [];
  let cursor: Parameters<Tx['scanEntries']>[2];
  do {
    const page = await api.commit((tx) => tx.scanEntries({ conversationId: api.conversationId, order: 'ascending' }, 200, cursor), ctx);
    for (const m of page.items.flatMap((e) => e.model ?? [])) {
      if (m.role === 'assistant') for (const c of m.content) if (c.type === 'toolCall' && c.name === 'bash') commands.set(c.id, String((c.arguments as { command?: unknown }).command ?? ''));
      if (m.role === 'toolResult' && commands.has(m.toolCallId)) runs.push({ command: commands.get(m.toolCallId)!, failed: m.isError });
    }
    cursor = page.next;
  } while (cursor);
  return runs;
}

/** Each claim the session's runs do not back; a failed run backs only a line that says it failed. */
export function unbackedClaims(text: string, runs: Run[], commits: string[]): string[] {
  const found = claimedCommands(text).flatMap(({ code, line }) => {
    const hits = runs.filter((r) => norm(r.command).includes(norm(code)));
    if (!hits.length) return [`${PR} says you ran \`${code}\`, but this session never ran it`];
    if (hits.every((r) => r.failed) && !/fail|red|error|broke/i.test(line)) return [`${PR} says \`${code}\` passed, but every run of it exited non-zero`];
    return [];
  });
  const missing = claimedShas(text).filter((s) => !commits.some((c) => c.startsWith(s)));
  return [...found, ...missing.map((s) => `${PR} names commit ${s}, which is not among the commits made since checkout`)];
}
