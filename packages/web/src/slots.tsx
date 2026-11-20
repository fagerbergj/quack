import { Fragment, type ReactNode } from 'react';
import { EXTENSIONS } from './extensions/index.ts';
import type { Row } from './sessions.ts';

const chips = EXTENSIONS.flatMap((e) => e.chip ?? []);
const providers = EXTENSIONS.flatMap((e) => e.provider ?? []);
const notices = EXTENSIONS.flatMap((e) => e.notice ?? []);
const rails = EXTENSIONS.flatMap((e) => e.rail ?? []);
const toolCalls = EXTENSIONS.flatMap((e) => e.toolCall ?? []);

/** The studio pane of the first extension that claims the session, if any. */
export const studioFor = (key: string) => EXTENSIONS.map((e) => e.studio).find((s) => s?.match(key));

/** Every extension's shared state around the shell, outermost first in registry order. */
export function ExtensionProviders({ rows, children }: { rows: Row[] | undefined; children: ReactNode }) {
  return providers.reduceRight<ReactNode>((inner, Provider) => <Provider rows={rows}>{inner}</Provider>, children);
}

/** The badges extensions draw beside a session's name. */
export function SessionChips({ sessionKey }: { sessionKey: string }) {
  return chips.map((Chip, i) => <Chip key={i} sessionKey={sessionKey} />);
}

/** What extensions say about the session list as a whole. */
export function ListNotices() {
  return notices.map((Notice, i) => <Notice key={i} />);
}

/** The extensions' sections at the top of a session's context rail. */
export function RailSections(props: { sessionKey: string; title: string; opening?: string; meta: readonly Record<string, unknown>[] }) {
  return rails.map((Rail, i) => <Rail key={i} {...props} />);
}

/** Extra UI extensions draw beneath a tool call row in the transcript. */
export function ToolCallExtra({ name, details }: { name: string; details: unknown }) {
  return toolCalls.flatMap((render, i) => {
    const node = render(name, details);
    return node == null ? [] : <Fragment key={i}>{node}</Fragment>;
  });
}
