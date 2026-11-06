import { EXTENSIONS } from './extensions/index.ts';
import type { View } from './view.ts';

type Args = Record<string, unknown>;
const file = (a: Args) => String(a.path ?? a.file_path ?? '').split('/').pop() || undefined;

// Core only knows the generic tools; an extension names the ones it owns.
const CORE: Record<string, (a: Args) => string> = {
  read: (a) => (file(a) ? `Reading ${file(a)}` : 'Reading a file'),
  bash: () => 'Running a command',
};

/** What a running session is doing, in plain words, from its in-flight tool call or generation. */
export function stepLabel(view: Pick<View, 'tool' | 'partial'>): string {
  const t = view.tool;
  if (t) {
    const args = (typeof t.args === 'object' && t.args ? t.args : {}) as Args;
    const own = EXTENSIONS.map((e) => e.steps?.[t.name]).find(Boolean) ?? CORE[t.name];
    return own?.(args) ?? 'Working';
  }
  const last = view.partial?.content.at(-1);
  return last?.type === 'thinking' ? 'Thinking' : last?.type === 'text' ? 'Writing' : 'Working';
}
